package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/proptrack/proptrack/internal/config"
	"github.com/proptrack/proptrack/internal/services"
)

// APIConfig must be set from main.go before the router starts.
var APIConfig *config.Config

// SourcesHealth handles GET /api/v1/sources/health
// Runs a live connectivity + auth probe for every configured external source
// and returns the aggregated results as a JSON array.
//
// Each probe has an individual 8–10 s timeout; the handler waits for all of
// them concurrently with a global 30 s deadline.
func SourcesHealth(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	results := services.CheckAPIHealth(ctx, APIConfig)
	c.JSON(http.StatusOK, gin.H{
		"sources":    results,
		"checked_at": time.Now().UTC(),
	})
}
