package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration sourced from environment variables.
type Config struct {
	// Dgraph
	DgraphAlphaHost string
	DgraphAlphaPort int

	// API
	APIPort       string
	APIAdminToken string
	LogLevel      string

	// AIS — aisstream.io WebSocket
	AISStreamAPIKey   string
	AISStreamEnabled  bool
	AISStreamURL      string
	AISWatchBoxes     []BoundingBox // geographic areas to monitor
	AISReconnectDelay time.Duration

	// Sanctions — OFAC SDN
	OFACEnabled         bool
	OFACSDNURL          string
	OFACUpdateInterval  time.Duration

	// Sanctions — UN Security Council Consolidated List
	UNSanctionsEnabled  bool
	UNSanctionsURL      string
	UNUpdateInterval    time.Duration

	// Sanctions — EU RELEX (optional, requires token)
	EUSanctionsEnabled bool
	EUSanctionsURL     string
	EUSanctionsToken   string
	EUUpdateInterval   time.Duration

	// Vessel cross-source verification
	VesselVerifyEnabled      bool
	VesselVerifyInterval     time.Duration
	VesselFinderEnabled      bool
	VesselFinderAPIKey       string
	MyShipTrackingEnabled    bool

	// Port surveillance — MarineTraffic vessel discovery
	MarineTrafficAPIKey  string
	MarineTrafficEnabled bool
	PortWatchInterval    time.Duration
	RoutePruneDays       int

	// MyShipTracking zone poller (discovery via /vessel/zone)
	MyShipTrackingAPIKey string
	MSTPollerEnabled     bool
	MSTPollerInterval    time.Duration

	// Datalastic enricher (discovery via /vessel_find + /vessel_pro)
	DatalasticAPIKey      string
	DatalasticEnabled     bool
	DatalasticInterval    time.Duration
	DatalasticEnrichBatch int // max vessels enriched via /vessel_pro per cycle

	// MarineTraffic web scraper (no API key — uses internal tile JSON endpoint)
	MTScraperEnabled  bool
	MTScraperInterval time.Duration

	// Global Fishing Watch — free API (registration at globalfishingwatch.org)
	GFWAPIKey   string
	GFWEnabled  bool
	GFWInterval time.Duration
}

// BoundingBox defines a geographic rectangle for AIS subscription.
// aisstream.io format: [[north_lat, west_lon], [south_lat, east_lon]]
type BoundingBox struct {
	Name     string
	NorthLat float64
	WestLon  float64
	SouthLat float64
	EastLon  float64
}

// DefaultWatchBoxes mirrors the 9 watchedPortZones plus the Hormuz chokepoint.
// Each box is tight around a specific port (≈1°×1°) to keep AISstream.io message
// volume within free-tier limits.  Large corridor boxes (China coast, Malacca,
// Indian Ocean) were generating thousands of messages/s and caused the server to
// close the connection with 1006 after ~2 minutes.
var DefaultWatchBoxes = []BoundingBox{
	// ── Chinese high-risk export ports ────────────────────────────────────────
	{Name: "cn_gaolan_zhuhai", NorthLat: 22.4, WestLon: 113.0, SouthLat: 21.6, EastLon: 113.7},
	{Name: "cn_taicang",       NorthLat: 32.0, WestLon: 120.6, SouthLat: 31.2, EastLon: 121.5},
	{Name: "cn_tianjin",       NorthLat: 39.5, WestLon: 117.2, SouthLat: 38.5, EastLon: 118.2},
	{Name: "cn_qingdao",       NorthLat: 36.6, WestLon: 119.8, SouthLat: 35.6, EastLon: 121.0},
	{Name: "cn_shanghai",      NorthLat: 32.0, WestLon: 121.0, SouthLat: 30.5, EastLon: 122.5},
	// ── Strait of Hormuz — compact chokepoint; all Iran-bound vessels pass here ─
	// At 5°×6° this is the one "transit" box kept; vessels here often declare
	// Iranian destinations in their AIS type-5 messages.
	{Name: "hormuz_chokepoint", NorthLat: 27.0, WestLon: 54.0, SouthLat: 22.0, EastLon: 60.0},
	// ── Iranian receiving ports ────────────────────────────────────────────────
	{Name: "ir_chabahar",     NorthLat: 25.8, WestLon: 60.2, SouthLat: 24.8, EastLon: 61.1},
	{Name: "ir_bandar_abbas", NorthLat: 27.6, WestLon: 55.8, SouthLat: 26.8, EastLon: 56.8},
	{Name: "ir_bushehr",      NorthLat: 29.5, WestLon: 50.3, SouthLat: 28.5, EastLon: 51.3},
	{Name: "ir_khorramshahr", NorthLat: 30.9, WestLon: 47.5, SouthLat: 29.9, EastLon: 48.9},
}

// Load reads configuration from environment variables with sane defaults.
func Load() *Config {
	alphaPort, _ := strconv.Atoi(getEnv("DGRAPH_ALPHA_PORT", "9080"))

	cfg := &Config{
		DgraphAlphaHost: getEnv("DGRAPH_ALPHA_HOST", "localhost"),
		DgraphAlphaPort: alphaPort,
		APIPort:         getEnv("API_PORT", "9090"),
		APIAdminToken:   getEnv("API_ADMIN_TOKEN", "changeme"),
		LogLevel:        getEnv("LOG_LEVEL", "info"),

		// ── AIS ──────────────────────────────────────────────────────────────
		AISStreamAPIKey:   getEnv("AIS_STREAM_API_KEY", ""),
		AISStreamEnabled:  getEnv("AIS_STREAM_API_KEY", "") != "",
		AISStreamURL:      getEnv("AIS_STREAM_URL", "wss://stream.aisstream.io/v0/stream"),
		AISReconnectDelay: parseDuration(getEnv("AIS_RECONNECT_DELAY", "30s")),
		AISWatchBoxes:     loadWatchBoxes(),

		// ── OFAC SDN ─────────────────────────────────────────────────────────
		OFACEnabled:        parseBool(getEnv("OFAC_ENABLED", "true")),
		OFACSDNURL:         getEnv("OFAC_SDN_URL", "https://www.treasury.gov/ofac/downloads/sdn.xml"),
		OFACUpdateInterval: parseDuration(getEnv("OFAC_UPDATE_INTERVAL", "24h")),

		// ── UN Sanctions ─────────────────────────────────────────────────────
		UNSanctionsEnabled: parseBool(getEnv("UN_SANCTIONS_ENABLED", "true")),
		UNSanctionsURL:     getEnv("UN_SANCTIONS_URL", "https://scsanctions.un.org/resources/xml/en/consolidated.xml"),
		UNUpdateInterval:   parseDuration(getEnv("UN_UPDATE_INTERVAL", "24h")),

		// ── EU RELEX (optional) ───────────────────────────────────────────────
		EUSanctionsEnabled: parseBool(getEnv("EU_SANCTIONS_ENABLED", "false")),
		EUSanctionsURL:     getEnv("EU_SANCTIONS_URL", "https://webgate.ec.europa.eu/europeaid/fsd/fsf/public/files/xmlFullSanctionsList_1_1/content"),
		EUSanctionsToken:   getEnv("EU_SANCTIONS_TOKEN", ""),
		EUUpdateInterval:   parseDuration(getEnv("EU_UPDATE_INTERVAL", "24h")),

		// ── Vessel cross-source verification ─────────────────────────────────
		VesselFinderAPIKey:    getEnv("VESSEL_FINDER_API_KEY", ""),
		VesselFinderEnabled:   getEnv("VESSEL_FINDER_API_KEY", "") != "",
		MyShipTrackingEnabled: parseBool(getEnv("MYSHIPTRACKING_ENABLED", "true")),
		VesselVerifyInterval:  parseDuration(getEnv("VESSEL_VERIFY_INTERVAL", "6h")),

		// ── MarineTraffic port surveillance ──────────────────────────────────
		MarineTrafficAPIKey:  getEnv("MARINETRAFFIC_API_KEY", ""),
		MarineTrafficEnabled: getEnv("MARINETRAFFIC_API_KEY", "") != "",
		PortWatchInterval:    parseDuration(getEnv("PORT_WATCH_INTERVAL", "1h")),
		RoutePruneDays:       parseInt(getEnv("ROUTE_PRUNE_DAYS", "30")),

		// ── MyShipTracking zone poller ────────────────────────────────────────
		MyShipTrackingAPIKey: getEnv("MYSHIPTRACKING_API_KEY", ""),
		MSTPollerEnabled:     getEnv("MYSHIPTRACKING_API_KEY", "") != "",
		MSTPollerInterval:    parseDuration(getEnv("MST_POLL_INTERVAL", "2h")),

		// ── Datalastic enricher ───────────────────────────────────────────────
		DatalasticAPIKey:      getEnv("DATALASTIC_API_KEY", ""),
		DatalasticEnabled:     getEnv("DATALASTIC_API_KEY", "") != "",
		DatalasticInterval:    parseDuration(getEnv("DATALASTIC_ENRICH_INTERVAL", "4h")),
		DatalasticEnrichBatch: parseInt(getEnv("DATALASTIC_ENRICH_BATCH", "25")),

		// ── MarineTraffic web scraper (no key required) ───────────────────────
		MTScraperEnabled:  parseBool(getEnv("MT_SCRAPER_ENABLED", "true")),
		MTScraperInterval: parseDuration(getEnv("MT_SCRAPE_INTERVAL", "3h")),

		// ── Global Fishing Watch ──────────────────────────────────────────────
		GFWAPIKey:   getEnv("GFW_API_KEY", ""),
		GFWEnabled:  getEnv("GFW_API_KEY", "") != "",
		GFWInterval: parseDuration(getEnv("GFW_POLL_INTERVAL", "6h")),
	}

	// EU requires a token; disable if not provided
	if cfg.EUSanctionsEnabled && cfg.EUSanctionsToken == "" {
		cfg.EUSanctionsEnabled = false
	}

	// Verifier is active if at least one source is enabled
	cfg.VesselVerifyEnabled = cfg.VesselFinderEnabled || cfg.MyShipTrackingEnabled

	return cfg
}

// loadWatchBoxes parses AIS_WATCH_AREAS env var (comma-separated area names)
// to select a subset of DefaultWatchBoxes. If unset, all defaults are used.
func loadWatchBoxes() []BoundingBox {
	areas := getEnv("AIS_WATCH_AREAS", "")
	if areas == "" {
		return DefaultWatchBoxes
	}
	selected := map[string]bool{}
	for _, a := range strings.Split(areas, ",") {
		selected[strings.TrimSpace(a)] = true
	}
	filtered := []BoundingBox{}
	for _, box := range DefaultWatchBoxes {
		if selected[box.Name] {
			filtered = append(filtered, box)
		}
	}
	if len(filtered) == 0 {
		return DefaultWatchBoxes
	}
	return filtered
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 24 * time.Hour
	}
	return d
}

func parseBool(s string) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false
	}
	return b
}

func parseInt(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 30
	}
	return n
}
