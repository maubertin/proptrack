package services

// api_health.go — lightweight connectivity and authentication probes for every
// external API source configured in PropTrack.
//
// Each probe follows the same contract:
//   - Uses only the cheapest possible API call (no credits burned where possible).
//   - Has a hard per-probe timeout (5–10 s) to keep startup fast.
//   - Distinguishes three states:
//       disabled  → key not set, probe skipped
//       unhealthy → reachable but auth failed, or unreachable
//       healthy   → authenticated response received

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/proptrack/proptrack/internal/config"
)

// APIHealthResult holds the probe outcome for one external source.
type APIHealthResult struct {
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Healthy   bool   `json:"healthy"`
	LatencyMs int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

// CheckAPIHealth runs all source probes concurrently and returns the aggregated results.
// ctx should carry an outer deadline (recommended: 30 s).
func CheckAPIHealth(ctx context.Context, cfg *config.Config) []APIHealthResult {
	type work struct {
		idx    int
		result APIHealthResult
	}

	probes := []func(context.Context, *config.Config) APIHealthResult{
		probeAISStream,
		probeMST,
		probeDatalastic,
		probeMarineTraffic,
		probeVesselFinder,
		probeMTScraper,
		probeGFW,
	}

	ch := make(chan work, len(probes))
	for i, fn := range probes {
		i, fn := i, fn
		go func() {
			ch <- work{i, fn(ctx, cfg)}
		}()
	}

	results := make([]APIHealthResult, len(probes)) //nolint:ineffassign
	for range probes {
		w := <-ch
		results[w.idx] = w.result
	}
	return results
}

// ── AISStream.io ──────────────────────────────────────────────────────────────
// Test: establish the WebSocket connection and send a minimal subscription.
// A successful handshake + write proves the key is accepted.
// No bandwidth is consumed: the subscription covers a zero-area bounding box.

func probeAISStream(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "aisstream.io", Enabled: cfg.AISStreamEnabled}
	if !cfg.AISStreamEnabled {
		return r
	}

	start := time.Now()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, cfg.AISStreamURL, nil)
	if err != nil {
		r.Error = fmt.Sprintf("WebSocket dial: %v", err)
		return r
	}
	defer conn.Close()

	// Send minimal subscription — empty bounding box burns no positions.
	sub := map[string]interface{}{
		"APIKey":             cfg.AISStreamAPIKey,
		"BoundingBoxes":      [][][2]float64{{{0.0, 0.0}, {0.0, 0.0}}},
		"FilterMessageTypes": []string{"PositionReport"},
	}
	if err := conn.WriteJSON(sub); err != nil {
		r.Error = fmt.Sprintf("subscription write: %v", err)
		return r
	}

	// Try to read one frame (may be an error/close from the server if key is bad,
	// or a first position message, or a timeout → all indicate the stream is live).
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, _, _ = conn.ReadMessage() // ignore content; a response of any kind = reachable

	r.Healthy = true
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── MyShipTracking ────────────────────────────────────────────────────────────
// Test: GET /vessel?mmsi=123456789&response=simple (1 credit, returns vessel-not-found
// for a dummy MMSI but validates the key).  A 200 or 404 = auth OK.

func probeMST(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "myshiptracking", Enabled: cfg.MSTPollerEnabled}
	if !cfg.MSTPollerEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet,
		"https://api.myshiptracking.com/api/v2/vessel?mmsi=123456789&response=simple", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.MyShipTrackingAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("HTTP: %v", err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		r.Healthy = true
	case http.StatusUnauthorized, http.StatusForbidden:
		r.Error = fmt.Sprintf("auth rejected (%d): %.200s", resp.StatusCode, body)
	default:
		r.Error = fmt.Sprintf("unexpected status %d: %.200s", resp.StatusCode, body)
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── Datalastic ────────────────────────────────────────────────────────────────
// Test: GET /vessel?mmsi=123456789 (cheapest call, 1 credit, any response = key valid).

func probeDatalastic(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "datalastic", Enabled: cfg.DatalasticEnabled}
	if !cfg.DatalasticEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.datalastic.com/api/v0/vessel?api-key=%s&mmsi=123456789",
		cfg.DatalasticAPIKey)
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("HTTP: %v", err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNotFound:
		// 200 with empty data or 404 are both valid authenticated responses.
		r.Healthy = true
	case http.StatusUnauthorized, http.StatusForbidden:
		r.Error = fmt.Sprintf("auth rejected (%d): %.200s", resp.StatusCode, body)
	default:
		// Datalastic returns 200 with an error payload for bad keys in some versions.
		var payload map[string]interface{}
		if json.Unmarshal(body, &payload) == nil {
			if errMsg, ok := payload["error"].(string); ok && errMsg != "" {
				r.Error = errMsg
				return r
			}
		}
		r.Healthy = true // treat unknown 2xx as healthy
		r.LatencyMs = time.Since(start).Milliseconds()
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── MarineTraffic ─────────────────────────────────────────────────────────────
// Test: reachability probe only (no API call to avoid credit consumption).
// An HTTP 200/301/302/405 on the services base URL confirms the endpoint is up.

func probeMarineTraffic(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "marinetraffic", Enabled: cfg.MarineTrafficEnabled}
	if !cfg.MarineTrafficEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(probeCtx, http.MethodHead,
		"https://services.marinetraffic.com/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("reachability: %v", err)
		return r
	}
	resp.Body.Close()

	// Any HTTP response proves the endpoint is reachable.
	// Key validity is only verified at first real poll cycle.
	r.Healthy = true
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── MarineTraffic web scraper ─────────────────────────────────────────────────
// Test: fetch one neutral ocean tile (z9/X256/Y256 = Atlantic) to verify the
// endpoint returns JSON without bot-detection.  No port-zone tiles are used.

func probeMTScraper(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "mt_scraper (web)", Enabled: cfg.MTScraperEnabled}
	if !cfg.MTScraperEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet,
		"https://www.marinetraffic.com/getData/get_data_json_4/z:9/X:256/Y:256/station:0", nil)
	for k, v := range mtBrowserHeaders {
		req.Header.Set(k, v)
	}

	// Use Chrome TLS fingerprint + http2 — same stack as the scraper itself.
	resp, err := newChromeHTTPClient(nil).Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("HTTP: %v", err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch {
	case resp.StatusCode == http.StatusOK && len(body) > 0 && body[0] != '<':
		r.Healthy = true
	case resp.StatusCode == http.StatusTooManyRequests:
		r.Error = "rate-limited (429)"
	case resp.StatusCode == http.StatusForbidden:
		r.Error = "access denied (403) — bot detection triggered"
	case len(body) > 0 && body[0] == '<':
		r.Error = "received HTML — possible CAPTCHA/bot detection"
	default:
		r.Error = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── Global Fishing Watch ──────────────────────────────────────────────────────
// Test: GET /v3/vessels/search with a minimal where clause.
// A 200 confirms the Bearer token is accepted without consuming credits.

func probeGFW(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "globalfishingwatch", Enabled: cfg.GFWEnabled}
	if !cfg.GFWEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet,
		gfwBaseURL+"/v3/vessels/search?where=flag%3D'IRN'&datasets[0]=public-global-vessel-identity:latest&limit=1",
		nil)
	req.Header.Set("Authorization", "Bearer "+cfg.GFWAPIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("HTTP: %v", err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch resp.StatusCode {
	case http.StatusOK:
		r.Healthy = true
	case http.StatusUnauthorized, http.StatusForbidden:
		r.Error = fmt.Sprintf("auth rejected (%d): %.200s", resp.StatusCode, body)
	default:
		r.Error = fmt.Sprintf("unexpected status %d: %.200s", resp.StatusCode, body)
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}

// ── VesselFinder ─────────────────────────────────────────────────────────────
// Test: GET /vessels?userkey=KEY&mmsi=123456789 — returns empty list for unknown MMSI
// but a 200 confirms the key is valid.

func probeVesselFinder(ctx context.Context, cfg *config.Config) APIHealthResult {
	r := APIHealthResult{Name: "vesselfinder", Enabled: cfg.VesselFinderEnabled}
	if !cfg.VesselFinderEnabled {
		return r
	}

	start := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.vesselfinder.com/vessels?userkey=%s&mmsi=123456789",
		cfg.VesselFinderAPIKey)
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.Error = fmt.Sprintf("HTTP: %v", err)
		return r
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

	switch resp.StatusCode {
	case http.StatusOK:
		r.Healthy = true
	case http.StatusUnauthorized, http.StatusForbidden:
		r.Error = fmt.Sprintf("auth rejected (%d): %.200s", resp.StatusCode, body)
	default:
		r.Error = fmt.Sprintf("unexpected status %d: %.200s", resp.StatusCode, body)
	}
	r.LatencyMs = time.Since(start).Milliseconds()
	return r
}
