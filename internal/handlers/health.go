package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Pinger reports whether a dependency answers; *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// healthTimeout bounds the database check so a stuck database fails the
// health check instead of hanging it.
const healthTimeout = 2 * time.Second

// Healthz handles GET /healthz for deploy checks and uptime monitors. It needs
// no authentication and reveals nothing beyond whether the service is up: 200
// "ok" when the database answers within healthTimeout, otherwise 503.
func Healthz(db Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthTimeout)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			c.String(http.StatusServiceUnavailable, "database unavailable")
			return
		}
		c.String(http.StatusOK, "ok")
	}
}
