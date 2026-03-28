package main

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/db"
	"github.com/proptrack/proptrack/internal/handlers"
	"github.com/proptrack/proptrack/internal/seed"
	"github.com/proptrack/proptrack/internal/services"
)

//go:embed web/index.html
var indexHTML []byte

func main() {
	// ── Config ────────────────────────────────────────────────────────────────
	cfg := config.Load()

	// ── Structured JSON logging ───────────────────────────────────────────────
	logLevel := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	slog.Info("PropTrack starting up", "version", "1.0.0")

	// ── Connect to Dgraph and apply schema ────────────────────────────────────
	_ = db.Client()

	if err := db.SetupSchema(); err != nil {
		slog.Error("schema setup failed", "err", err)
		os.Exit(1)
	}

	// ── Seed known entities ───────────────────────────────────────────────────
	seedCtx, seedCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := seed.Run(seedCtx); err != nil {
		slog.Warn("seed data partially failed", "err", err)
	}
	if err := seed.SeedShipments(seedCtx); err != nil {
		slog.Warn("seed shipments partially failed", "err", err)
	}
	updated, failed := services.RecomputeAllActive(seedCtx)
	slog.Info("startup score recompute", "updated", updated, "failed", failed)
	seedCancel()

	// ── Background services ───────────────────────────────────────────────────
	// Root context cancelled on SIGINT/SIGTERM for clean shutdown
	rootCtx, rootCancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer rootCancel()

	// AIS poller — aisstream.io WebSocket (enabled when AIS_STREAM_API_KEY is set)
	aisPoller := services.NewAISPoller(cfg)
	go aisPoller.Start(rootCtx)

	// Sanctions updater — OFAC SDN + UN SC + EU RELEX
	sanctionsUpdater := services.NewSanctionsUpdater(cfg)
	go sanctionsUpdater.Start(rootCtx)

	// Vessel verifier — cross-source identity check (VesselFinder + MyShipTracking)
	verifier := services.NewVesselVerifier(cfg)
	handlers.Verifier = verifier
	handlers.APIConfig = cfg
	go verifier.Start(rootCtx)

	// API source health diagnostic — runs once at startup, results logged via slog
	go func() {
		diagCtx, diagCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer diagCancel()
		results := services.CheckAPIHealth(diagCtx, cfg)
		for _, r := range results {
			if !r.Enabled {
				slog.Info("API source disabled", "name", r.Name)
			} else if r.Healthy {
				slog.Info("API source healthy", "name", r.Name, "latency_ms", r.LatencyMs)
			} else {
				slog.Warn("API source unhealthy", "name", r.Name, "error", r.Error)
			}
		}
	}()

	// Port surveillance — MarineTraffic discovery + route pruning
	portWatcher := services.NewPortWatcher(cfg)
	go portWatcher.Start(rootCtx)

	// MyShipTracking zone poller — complementary bounding-box discovery
	mstPoller := services.NewMSTPoller(cfg)
	go mstPoller.Start(rootCtx)

	// Datalastic enricher — flag-based discovery + active vessel enrichment
	dlEnricher := services.NewDatalasticEnricher(cfg)
	go dlEnricher.Start(rootCtx)

	// MarineTraffic web scraper — tile-based discovery (no API key)
	mtScraper := services.NewMTWebScraper(cfg)
	go mtScraper.Start(rootCtx)

	// Global Fishing Watch — flag-based discovery + Iranian port visit detection
	gfwPoller := services.NewGFWPoller(cfg)
	go gfwPoller.Start(rootCtx)

	// Hourly score refresh
	go services.StartHourlyRefresh(rootCtx)

	// ── Gin router ────────────────────────────────────────────────────────────
	if cfg.LogLevel != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(jsonLogger())

	// Frontend SPA
	r.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})

	// Health — also exposes poller status
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":              "ok",
			"time":                time.Now().UTC(),
			"ais_enabled":         cfg.AISStreamEnabled,
			"ofac_enabled":        cfg.OFACEnabled,
			"un_enabled":          cfg.UNSanctionsEnabled,
			"eu_enabled":          cfg.EUSanctionsEnabled,
			"port_watch_enabled":  cfg.MarineTrafficEnabled,
			"mst_enabled":         cfg.MSTPollerEnabled,
			"datalastic_enabled":  cfg.DatalasticEnabled,
			"mt_scraper_enabled":  cfg.MTScraperEnabled,
			"gfw_enabled":         cfg.GFWEnabled,
		})
	})

	v1 := r.Group("/api/v1")

	// Vessels
	vessels := v1.Group("/vessels")
	{
		vessels.POST("", handlers.CreateVessel)
		vessels.GET("/sanctioned", handlers.ListSanctionedVessels)
		vessels.GET("/dark", handlers.ListDarkVessels)
		vessels.GET("/:imo", handlers.GetVessel)
		vessels.PUT("/:imo/position", handlers.UpdateVesselPosition)
		vessels.POST("/:imo/verify", handlers.VerifyVessel)
	}

	// Shipments
	shipments := v1.Group("/shipments")
	{
		shipments.POST("", handlers.CreateShipment)
		shipments.GET("/active", handlers.ListActiveShipments)
		shipments.GET("/critical", handlers.ListCriticalShipments)
		shipments.GET("/:id", handlers.GetShipment)
		shipments.PUT("/:id/status", handlers.UpdateShipmentStatus)
		shipments.POST("/:id/waypoint", handlers.AddWaypoint)
	}

	// Scoring
	scoring := v1.Group("/score")
	{
		scoring.POST("/vessel/:imo", handlers.RecomputeVesselScore)
		scoring.POST("/shipment/:id", handlers.RecomputeShipmentScore)
		scoring.POST("/recompute-all", handlers.RecomputeAllShipments)
		scoring.GET("/leaderboard", handlers.ScoreLeaderboard)
	}

	// Graph queries
	graph := v1.Group("/graph")
	{
		graph.GET("/vessel/:imo/connections", handlers.VesselConnections)
		graph.GET("/company/:name/fleet", handlers.CompanyFleet)
		graph.POST("/query", handlers.RawDQLQuery)
	}

	// Ports
	ports := v1.Group("/ports")
	{
		ports.POST("", handlers.UpsertPort)
		ports.GET("/risk/:level", handlers.PortsByRisk)
		ports.GET("/:unlocode/shipments", handlers.PortShipments)
	}

	// Map / visualization
	mapGroup := v1.Group("/map")
	{
		mapGroup.GET("/vessels", handlers.MapVessels)
		mapGroup.GET("/shipment/:id/track", handlers.MapShipmentTrack)
		mapGroup.GET("/overview", handlers.MapOverview)
	}

	// Sources health — live connectivity + auth probe for all configured APIs
	v1.GET("/sources/health", handlers.SourcesHealth)

	// ── Start HTTP server ─────────────────────────────────────────────────────
	addr := ":" + cfg.APIPort
	srv := &http.Server{Addr: addr, Handler: r}

	go func() {
		slog.Info("PropTrack API listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	<-rootCtx.Done()
	slog.Info("shutdown signal received")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	slog.Info("PropTrack stopped")
}

// jsonLogger returns a Gin middleware that emits structured request logs via slog.
func jsonLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		slog.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"ip", c.ClientIP(),
		)
	}
}
