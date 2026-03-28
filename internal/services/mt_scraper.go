package services

// mt_scraper.go — MarineTraffic public web interface scraper.
//
// No API key required.  Uses the internal JSON tile endpoint that backs MT's
// own map view.  The endpoint is undocumented and may change; parse errors are
// logged with a raw-response excerpt so operators can adapt quickly.
//
// Endpoint pattern:
//   GET https://www.marinetraffic.com/getData/get_data_json_4/z:{zoom}/X:{x}/Y:{y}/station:0
//
// Geographic coverage:  same 9 port zones as the other pollers.
// Zoom level 9 is used (~78 km × 78 km per tile at mid-latitudes).
// Each zone bounding box is covered by the minimal set of overlapping tiles.
//
// Rate policy:
//   • 1.5 s sleep between successive tile requests.
//   • One automatic retry on network errors (no retry on 4xx/5xx).
//   • Cycle cadence: configurable via MT_SCRAPE_INTERVAL (default 3 h).
//
// Enrichment and scoring are NOT performed here: discovered vessels are upserted
// as Vessel nodes and linked to a monitoring Shipment; the hourly score refresh
// then evaluates them.

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"github.com/proptrack/proptrack/internal/config"
)

// ── Tile maths ────────────────────────────────────────────────────────────────

// latLonToTile converts geographic coordinates to slippy map tile numbers.
// Standard OSM / WebMercator formula.
func latLonToTile(lat, lon float64, zoom int) (x, y int) {
	n := math.Pow(2, float64(zoom))
	x = int(math.Floor((lon + 180.0) / 360.0 * n))
	latRad := lat * math.Pi / 180.0
	y = int(math.Floor((1.0 - math.Log(math.Tan(latRad)+1.0/math.Cos(latRad))/math.Pi) / 2.0 * n))
	return
}

// zoneTiles returns all tile coordinates that overlap a watchedPortZone bounding box.
func zoneTiles(zone watchedPortZone, zoom int) [][2]int {
	// NB: tile Y increases southward, so MaxLat → smaller Y.
	x1, y1 := latLonToTile(zone.MaxLat, zone.MinLon, zoom)
	x2, y2 := latLonToTile(zone.MinLat, zone.MaxLon, zoom)
	var tiles [][2]int
	for tx := x1; tx <= x2; tx++ {
		for ty := y1; ty <= y2; ty++ {
			tiles = append(tiles, [2]int{tx, ty})
		}
	}
	return tiles
}

// ── Response parsing ──────────────────────────────────────────────────────────

// mtTileVesselObj is the named-field format used by recent MT tile responses.
// Fields tagged with *10 are fixed-point (divide by 10 to get the real value).
type mtTileVesselObj struct {
	MMSI        string  `json:"MMSI"`
	IMO         string  `json:"IMO"`
	ShipName    string  `json:"SHIPNAME"`
	Flag        string  `json:"FLAG"`
	ShipType    int     `json:"SHIPTYPE"`
	Lat         float64 `json:"LAT"`   // direct degrees
	Lon         float64 `json:"LON"`   // direct degrees
	Speed       float64 `json:"SPEED"` // knots×10 or direct — normalised below
	Heading     float64 `json:"HEADING"`
	Status      int     `json:"STATUS"`
	Draught     float64 `json:"DRAUGHT"` // metres×10 or direct — normalised below
	Destination string  `json:"DESTINATION"`
	ETA         string  `json:"ETA"`
	Length      float64 `json:"LENGTH"`
	Width       float64 `json:"WIDTH"`
	GT          float64 `json:"GT"`
	DWT         float64 `json:"DWT"`
}

// normaliseMTVessel applies fixed-point correction heuristics for SPEED and DRAUGHT.
// MT sometimes returns SPEED as knots×10 (e.g. 85 = 8.5 kn) and DRAUGHT as m×10.
func normaliseMTVessel(v *mtTileVesselObj) {
	if v.Speed > 50 { // unrealistic raw knot value → divide by 10
		v.Speed /= 10
	}
	if v.Draught > 30 { // draught > 30 m is impossible → divide by 10
		v.Draught /= 10
	}
}

// parseMTTileResponse parses a raw tile API body into a slice of vessels.
// Handles both the named-field object format and the legacy compact-array format.
// Returns an empty slice (not an error) on an empty but valid response.
func parseMTTileResponse(body []byte) ([]mtTileVesselObj, error) {
	// Attempt 1: {"data":{"rows":[{...}, ...]}}
	var wrapper struct {
		Data struct {
			Rows json.RawMessage `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		return nil, fmt.Errorf("outer unmarshal: %w", err)
	}
	if len(wrapper.Data.Rows) == 0 {
		return nil, nil
	}

	// Try object-array format first (most common in current MT versions).
	var objs []mtTileVesselObj
	if err := json.Unmarshal(wrapper.Data.Rows, &objs); err == nil {
		for i := range objs {
			normaliseMTVessel(&objs[i])
		}
		return objs, nil
	}

	// Attempt 2: rows is an array of arrays — use positional mapping.
	// Known column order (may vary): MMSI, ShipID, LAT, LON, SPEED, HEADING,
	// COURSE, STATUS, RECEIVED_AGO, SHIPNAME, SHIPTYPE, FLAG, DRAUGHT,
	// DESTINATION, ETA, LENGTH, WIDTH, GT, DWT, IMO.
	var rawRows [][]json.RawMessage
	if err := json.Unmarshal(wrapper.Data.Rows, &rawRows); err != nil {
		return nil, fmt.Errorf("rows format unknown (not object-array nor array-array): %.120s", body)
	}

	var result []mtTileVesselObj
	for _, row := range rawRows {
		if len(row) < 20 {
			continue
		}
		v := mtTileVesselObj{}
		jsonStr := func(r json.RawMessage) string {
			var s string
			if json.Unmarshal(r, &s) == nil {
				return s
			}
			return strings.Trim(string(r), `"`)
		}
		jsonFloat := func(r json.RawMessage) float64 {
			var f float64
			_ = json.Unmarshal(r, &f)
			return f
		}
		jsonInt := func(r json.RawMessage) int {
			var i int
			_ = json.Unmarshal(r, &i)
			return i
		}

		v.MMSI = jsonStr(row[0])
		v.Lat = jsonFloat(row[2])
		v.Lon = jsonFloat(row[3])
		v.Speed = jsonFloat(row[4])
		v.Heading = jsonFloat(row[5])
		v.Status = jsonInt(row[7])
		v.ShipName = jsonStr(row[9])
		v.ShipType = jsonInt(row[10])
		v.Flag = jsonStr(row[11])
		v.Draught = jsonFloat(row[12])
		v.Destination = jsonStr(row[13])
		v.ETA = jsonStr(row[14])
		v.Length = jsonFloat(row[15])
		v.Width = jsonFloat(row[16])
		v.GT = jsonFloat(row[17])
		v.DWT = jsonFloat(row[18])
		if len(row) > 19 {
			v.IMO = jsonStr(row[19])
		}
		normaliseMTVessel(&v)
		result = append(result, v)
	}
	return result, nil
}

// ── Scraper ───────────────────────────────────────────────────────────────────

const mtScraperZoom = 9 // ~78 km × 78 km per tile; 4–9 tiles per port zone

// errMTBlocked is returned by fetchTile when all endpoint variants return 403.
// It signals a session/bot-detection block so the caller can abort the cycle
// immediately rather than hammering every tile and flooding logs with WARNs.
var errMTBlocked = fmt.Errorf("marinetraffic: blocked by bot detection (403)")

// mtBrowserHeaders is sent with every tile (XHR) request.
// These mirror exactly what Chrome 124 sends for a same-origin AJAX call.
var mtBrowserHeaders = map[string]string{
	"User-Agent":       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Accept":           "application/json, text/javascript, */*; q=0.01",
	"Accept-Language":  "en-US,en;q=0.9",
	// Explicit Accept-Encoding is required: MarineTraffic's WAF checks for it as
	// part of the browser fingerprint.  We handle decompression manually in fetchTile
	// for both gzip and brotli (andybalholm/brotli is already a transitive dependency).
	"Accept-Encoding":  "gzip, deflate, br",
	"Referer":          "https://www.marinetraffic.com/",
	"Origin":           "https://www.marinetraffic.com",
	"X-Requested-With": "XMLHttpRequest",
	"sec-ch-ua":         `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
	"sec-ch-ua-mobile":  "?0",
	"sec-ch-ua-platform": `"macOS"`,
	"sec-fetch-dest":   "empty",
	"sec-fetch-mode":   "cors",
	"sec-fetch-site":   "same-origin",
	"Pragma":           "no-cache",
	"Cache-Control":    "no-cache",
}

// mtNavHeaders is used for the session warmup (initial page-navigation request).
// Cloudflare distinguishes document navigation from XHR — these must differ.
var mtNavHeaders = map[string]string{
	"User-Agent":       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Accept":           "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
	"Accept-Language":  "en-US,en;q=0.9",
	"Accept-Encoding":  "gzip, deflate, br",
	"sec-ch-ua":         `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
	"sec-ch-ua-mobile":  "?0",
	"sec-ch-ua-platform": `"macOS"`,
	"sec-fetch-dest":    "document",
	"sec-fetch-mode":    "navigate",
	"sec-fetch-site":    "none",
	"sec-fetch-user":    "?1",
	"Upgrade-Insecure-Requests": "1",
}

// mtTileEndpoints lists tile API paths to try in order.
// MT has silently versioned this endpoint several times; we fall back through known variants.
var mtTileEndpoints = []string{
	"https://www.marinetraffic.com/getData/get_data_json_4/z:%d/X:%d/Y:%d/station:0",
	"https://www.marinetraffic.com/getData/get_data_json_3/z:%d/X:%d/Y:%d/station:0",
}

// MTWebScraper scrapes MarineTraffic's public tile JSON endpoint.
type MTWebScraper struct {
	cfg    *config.Config
	client *http.Client // persistent client with cookie jar
}

// dialChromeTLS opens a TCP connection and performs a TLS handshake using utls
// with Chrome 120's exact ClientHello spec.  The returned connection properly
// exposes ConnectionState() so the http2 layer can detect the negotiated ALPN.
func dialChromeTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	rawConn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(rawConn, &utls.Config{
		ServerName:   host,
		OmitEmptyPsk: true,
	}, utls.HelloChrome_120)
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return uconn, nil
}

// newChromeHTTPClient returns an http.Client that:
//   - Spoofs Chrome 120's TLS JA3 fingerprint via utls (bypasses Cloudflare bot detection)
//   - Uses golang.org/x/net/http2.Transport directly so HTTP/2 frames are handled
//     correctly.  ConfigureTransport cannot be used here because it requires the
//     connection to be *tls.Conn; utls returns *utls.UConn which fails that assertion
//     and causes "malformed HTTP response" errors when the server sends HTTP/2 frames.
func newChromeHTTPClient(jar http.CookieJar) *http.Client {
	t2 := &http2.Transport{
		// DialTLSContext signature for http2.Transport includes the *tls.Config arg
		// which we ignore — utls handles TLS entirely.
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dialChromeTLS(ctx, network, addr)
		},
		// Allow the h2 transport to be used for cleartext too (not needed here but
		// avoids panics if the server downgrades the ALPN to "http/1.1").
		AllowHTTP: false,
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Jar:       jar,
		Transport: t2,
	}
}

// NewMTWebScraper creates a new MTWebScraper.  The cookie jar is initialised
// once; the same session is reused across requests in the same process lifetime.
func NewMTWebScraper(cfg *config.Config) *MTWebScraper {
	jar, _ := cookiejar.New(nil)
	return &MTWebScraper{
		cfg:    cfg,
		client: newChromeHTTPClient(jar),
	}
}

// Start launches the scrape loop.  Blocks until ctx is cancelled.
func (s *MTWebScraper) Start(ctx context.Context) {
	if !s.cfg.MTScraperEnabled {
		slog.Warn("MT web scraper disabled (MT_SCRAPER_ENABLED=false)")
		return
	}
	slog.Info("MT web scraper started",
		"interval", s.cfg.MTScraperInterval,
		"zoom", mtScraperZoom,
	)

	s.runCycle(ctx)

	ticker := time.NewTicker(s.cfg.MTScraperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.runCycle(ctx)
		case <-ctx.Done():
			slog.Info("MT web scraper stopped")
			return
		}
	}
}

// warmupSession performs a browser-like page navigation to marinetraffic.com so that
// Cloudflare sets its session cookies before any XHR tile requests are made.
// The response body is discarded; only cookies matter.
func (s *MTWebScraper) warmupSession(ctx context.Context) {
	warmCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(warmCtx, http.MethodGet, "https://www.marinetraffic.com/", nil)
	if err != nil {
		slog.Debug("MT scraper: warmup request build failed", "err", err)
		return
	}
	for k, v := range mtNavHeaders {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		slog.Debug("MT scraper: warmup failed", "err", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection is reusable
	slog.Debug("MT scraper: session warmed up", "status", resp.StatusCode, "cookies", len(s.client.Jar.Cookies(req.URL)))
}

// runCycle iterates all watched port zones, scrapes vessels, and upserts records.
func (s *MTWebScraper) runCycle(ctx context.Context) {
	s.warmupSession(ctx)
	slog.Info("MT scraper: starting cycle", "zones", len(watchedPortZones))
	discovered := 0

	for _, zone := range watchedPortZones {
		vessels, err := s.scrapeZone(ctx, zone)
		if err != nil {
			if err == errMTBlocked {
				slog.Warn("MT scraper: cycle aborted — bot detection active (cf_clearance required); will retry next cycle",
					"next_attempt_in", s.cfg.MTScraperInterval)
				return
			}
			slog.Warn("MT scraper: zone scrape failed", "zone", zone.UNLOCODE, "err", err)
			continue
		}
		slog.Debug("MT scraper: zone scraped", "zone", zone.UNLOCODE, "vessels", len(vessels))

		for _, v := range vessels {
			if v.IMO == "" || v.IMO == "0" {
				continue
			}
			switch zone.Country {
			case "CN":
				if s.processCNZone(ctx, v, zone) {
					discovered++
				}
			case "IR":
				s.processIRZone(ctx, v, zone)
			}
		}
	}

	slog.Info("MT scraper: cycle complete", "new_shipments", discovered)

	pruned := osintPruneNonIranRoutes(ctx, s.cfg.RoutePruneDays)
	if pruned > 0 {
		slog.Info("MT scraper: routes pruned", "count", pruned)
	}
}

// scrapeZone fetches all tiles that cover zone and merges the vessel lists.
// Deduplication by IMO is applied to remove duplicates from overlapping tiles.
func (s *MTWebScraper) scrapeZone(ctx context.Context, zone watchedPortZone) ([]mtTileVesselObj, error) {
	tiles := zoneTiles(zone, mtScraperZoom)
	seen := make(map[string]struct{})
	var merged []mtTileVesselObj

	for i, tile := range tiles {
		if i > 0 {
			select {
			case <-ctx.Done():
				return merged, ctx.Err()
			case <-time.After(1500 * time.Millisecond):
			}
		}

		vessels, err := s.fetchTile(ctx, tile[0], tile[1])
		if err != nil {
			if err == errMTBlocked {
				return nil, errMTBlocked // abort zone; caller will abort cycle
			}
			slog.Warn("MT scraper: tile fetch failed",
				"zone", zone.UNLOCODE, "x", tile[0], "y", tile[1], "err", err)
			continue
		}

		for _, v := range vessels {
			key := v.IMO
			if key == "" {
				key = v.MMSI
			}
			if key == "" || key == "0" {
				continue
			}
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				merged = append(merged, v)
			}
		}
	}
	return merged, nil
}

// fetchTile calls MT's tile JSON endpoint and parses the response.
// Tries each URL pattern in mtTileEndpoints in order; falls back on 403/404.
// Returns an empty slice (not an error) when the tile is valid but empty.
func (s *MTWebScraper) fetchTile(ctx context.Context, x, y int) ([]mtTileVesselObj, error) {
	var lastErr error
	for _, pattern := range mtTileEndpoints {
		tileURL := fmt.Sprintf(pattern, mtScraperZoom, x, y)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, tileURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range mtBrowserHeaders {
			req.Header.Set(k, v)
		}

		// One retry on transient network error.
		var resp *http.Response
		for attempt := 0; attempt < 2; attempt++ {
			resp, err = s.client.Do(req)
			if err == nil {
				break
			}
			if attempt == 0 {
				slog.Debug("MT scraper: tile request failed, retrying", "err", err)
				time.Sleep(3 * time.Second)
			}
		}
		if err != nil {
			lastErr = fmt.Errorf("tile request: %w", err)
			continue
		}

		// Decompress manually: we set Accept-Encoding explicitly (required for WAF
		// fingerprinting), which disables the transport's automatic decompression.
		var bodyReader io.Reader = resp.Body
		switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
		case "gzip":
			gr, gerr := gzip.NewReader(resp.Body)
			if gerr != nil {
				resp.Body.Close()
				lastErr = fmt.Errorf("gzip reader: %w", gerr)
				continue
			}
			defer gr.Close()
			bodyReader = gr
		case "br":
			bodyReader = brotli.NewReader(resp.Body)
		}
		body, err := io.ReadAll(io.LimitReader(bodyReader, 2<<20)) // 2 MB cap
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read body: %w", err)
			continue
		}

		switch resp.StatusCode {
		case http.StatusOK:
			// Happy path — fall through to parse.
		case http.StatusTooManyRequests:
			return nil, fmt.Errorf("rate-limited (429); back off before next cycle")
		case http.StatusForbidden, http.StatusUnauthorized:
			// 403 on all variants = bot-detection block; signal to abort the cycle.
			lastErr = errMTBlocked
			continue
		case http.StatusNotFound:
			lastErr = fmt.Errorf("endpoint %s returned 404", pattern)
			continue
		default:
			lastErr = fmt.Errorf("unexpected status %d from %s", resp.StatusCode, pattern)
			continue
		}

		// Detect HTML anti-bot / CAPTCHA pages.
		if len(body) > 10 && body[0] == '<' {
			lastErr = fmt.Errorf("received HTML instead of JSON from %s — possible CAPTCHA/bot detection", pattern)
			continue
		}

		vessels, err := parseMTTileResponse(body)
		if err != nil {
			excerpt := body
			if len(excerpt) > 300 {
				excerpt = excerpt[:300]
			}
			return nil, fmt.Errorf("parse error: %w — raw: %.300s", err, excerpt)
		}
		return vessels, nil
	}

	// All endpoint variants exhausted.
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("all tile endpoint variants returned errors")
}

// ── Post-processing ───────────────────────────────────────────────────────────

func (s *MTWebScraper) processCNZone(ctx context.Context, v mtTileVesselObj, zone watchedPortZone) bool {
	if v.Speed > 14 {
		return false
	}

	vesselUID, err := osintUpsertVessel(ctx,
		v.IMO, v.MMSI, v.ShipName, v.Flag,
		mtShipTypeToInternal(v.ShipType), "mt_scraper",
		v.Lat, v.Lon, v.Draught, v.GT, v.DWT,
	)
	if err != nil || vesselUID == "" {
		slog.Warn("MT scraper: vessel upsert failed", "imo", v.IMO, "err", err)
		return false
	}

	if osintHasRecentShipmentFromPort(ctx, v.IMO, zone.UNLOCODE, 60) {
		return false
	}

	destUNLOCODE := parseIranianDestinationPW(v.Destination)
	status := "monitoring"
	if destUNLOCODE != "" {
		status = "active"
	}

	originUID, err := pwResolvePort(ctx, zone.UNLOCODE)
	if err != nil || originUID == "" {
		slog.Warn("MT scraper: origin port not found", "unlocode", zone.UNLOCODE)
		return false
	}

	destUID := ""
	if destUNLOCODE != "" {
		destUID, _ = pwResolvePort(ctx, destUNLOCODE)
	}

	shipmentID := "MTS-" + uuid.New().String()[:8]
	sourceRef := fmt.Sprintf(
		"Auto-discovered via MarineTraffic web scraper (tile z%d); AIS dest: %q; speed: %.1f kn",
		mtScraperZoom, v.Destination, v.Speed,
	)
	if err := osintCreateShipment(ctx, shipmentID, vesselUID, originUID, destUID, status, sourceRef); err != nil {
		slog.Warn("MT scraper: shipment creation failed", "imo", v.IMO, "err", err)
		return false
	}
	slog.Info("MT scraper: shipment auto-created",
		"id", shipmentID, "imo", v.IMO, "vessel", v.ShipName,
		"origin", zone.UNLOCODE, "dest", destUNLOCODE, "status", status,
	)
	return true
}

func (s *MTWebScraper) processIRZone(ctx context.Context, v mtTileVesselObj, zone watchedPortZone) {
	osintUpsertIranPresence(ctx,
		v.IMO, v.MMSI, v.ShipName, v.Flag, mtShipTypeToInternal(v.ShipType), "mt_scraper",
		v.Lat, v.Lon, v.Draught, v.GT, v.DWT,
		zone.UNLOCODE, "MTS",
	)
}

// mtShipTypeToInternal maps MT numeric ship type codes to internal type strings.
// Reference: https://api.marinetraffic.com/en/api/show/vessels (type table)
func mtShipTypeToInternal(t int) string {
	switch {
	case t >= 70 && t <= 79: // General Cargo
		return "general_cargo"
	case t >= 80 && t <= 89: // Tanker
		return "chemical_tanker"
	case t == 70, t == 72: // Cargo
		return "general_cargo"
	case t >= 60 && t <= 69: // Passenger
		return "general_cargo"
	case t == 1, t == 4: // WIG, HSC
		return "general_cargo"
	default:
		if t >= 40 && t <= 49 { // High-speed craft
			return "general_cargo"
		}
		return "bulk_carrier"
	}
}
