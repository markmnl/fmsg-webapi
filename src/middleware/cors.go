package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// CORSConfig configures the CORS middleware.
type CORSConfig struct {
	// AllowedOrigins is the list of exact origins permitted to access the API
	// from a browser, e.g. "https://fmsg.io". A single entry of "*" allows any
	// origin (only valid when credentials are not used). An empty list
	// disables CORS entirely.
	AllowedOrigins []string
	// AllowedMethods are the HTTP methods returned in the preflight response.
	AllowedMethods []string
	// AllowedHeaders are the request headers returned in the preflight response.
	AllowedHeaders []string
	// MaxAge controls how long browsers may cache the preflight result.
	MaxAge time.Duration
}

// DefaultCORSConfig returns a CORSConfig populated with values appropriate for
// this API: GET/POST/PUT/DELETE/OPTIONS plus Authorization and Content-Type
// request headers, with a 10 minute preflight cache. Callers must still set
// AllowedOrigins.
func DefaultCORSConfig() CORSConfig {
	return CORSConfig{
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Content-Type"},
		MaxAge:         10 * time.Minute,
	}
}

// NewCORS returns a Gin middleware that handles CORS preflight requests and
// adds the Access-Control-Allow-* headers to matching cross-origin responses.
//
// Behaviour:
//   - Requests without an Origin header pass through untouched.
//   - When Origin matches an entry in AllowedOrigins (or AllowedOrigins is
//     {"*"}), the appropriate Access-Control-Allow-* headers are added.
//   - OPTIONS preflight requests are short-circuited with 204 so they never
//     reach downstream auth middleware (which would reject them for missing
//     the Authorization header).
//   - When Origin is present but not allowed, the request is allowed to
//     continue without CORS headers; the browser will then block the
//     response, which is the standard CORS failure mode.
func NewCORS(cfg CORSConfig) gin.HandlerFunc {
	if len(cfg.AllowedOrigins) == 0 {
		// CORS disabled; return a no-op middleware.
		return func(c *gin.Context) { c.Next() }
	}

	trimmedOrigins := make([]string, 0, len(cfg.AllowedOrigins))
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		origin := strings.TrimSpace(o)
		if origin == "" {
			continue
		}
		trimmedOrigins = append(trimmedOrigins, origin)
		allowed[origin] = struct{}{}
	}
	if len(trimmedOrigins) == 0 {
		// CORS disabled; return a no-op middleware.
		return func(c *gin.Context) { c.Next() }
	}
	allowAny := len(trimmedOrigins) == 1 && trimmedOrigins[0] == "*"

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	maxAge := strconv.Itoa(int(cfg.MaxAge.Seconds()))

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			c.Next()
			return
		}

		// Always advertise that the response varies by Origin so caches
		// (browser + intermediaries) don't serve a response keyed only on
		// the URL across different origins.
		c.Writer.Header().Add("Vary", "Origin")

		_, ok := allowed[origin]
		if !ok && !allowAny {
			// Not an allowed origin. Don't add CORS headers; let the request
			// proceed (the browser will block the response).
			c.Next()
			return
		}

		if allowAny {
			c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		} else {
			c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
		}

		if c.Request.Method == http.MethodOptions {
			// Preflight.
			c.Writer.Header().Add("Vary", "Access-Control-Request-Method")
			c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
			if methods != "" {
				c.Writer.Header().Set("Access-Control-Allow-Methods", methods)
			}
			if headers != "" {
				c.Writer.Header().Set("Access-Control-Allow-Headers", headers)
			}
			if cfg.MaxAge > 0 {
				c.Writer.Header().Set("Access-Control-Max-Age", maxAge)
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
