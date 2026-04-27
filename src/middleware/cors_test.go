package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newCORSTestRouter(origins []string) *gin.Engine {
	r := gin.New()
	cfg := DefaultCORSConfig()
	cfg.AllowedOrigins = origins
	r.Use(NewCORS(cfg))
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return r
}

func TestCORS_NoOriginPassesThrough(t *testing.T) {
	r := newCORSTestRouter([]string{"https://fmsg.io"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORS_AllowedOriginGetsHeaders(t *testing.T) {
	r := newCORSTestRouter([]string{"https://fmsg.io"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://fmsg.io")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://fmsg.io" {
		t.Errorf("Access-Control-Allow-Origin = %q, want https://fmsg.io", got)
	}
	if got := w.Header().Get("Vary"); got == "" {
		t.Errorf("Vary header missing")
	}
}

func TestCORS_DisallowedOriginGetsNoHeaders(t *testing.T) {
	r := newCORSTestRouter([]string{"https://fmsg.io"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORS_PreflightShortCircuits(t *testing.T) {
	r := gin.New()
	cfg := DefaultCORSConfig()
	cfg.AllowedOrigins = []string{"https://fmsg.io"}
	r.Use(NewCORS(cfg))
	// Downstream middleware that would reject if reached.
	r.Use(func(c *gin.Context) {
		c.AbortWithStatus(http.StatusUnauthorized)
	})
	r.POST("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/x", nil)
	req.Header.Set("Origin", "https://fmsg.io")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization, Content-Type")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://fmsg.io" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Errorf("Access-Control-Allow-Methods missing")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Errorf("Access-Control-Allow-Headers missing")
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Errorf("Access-Control-Max-Age missing")
	}
}

func TestCORS_Wildcard(t *testing.T) {
	r := newCORSTestRouter([]string{"*"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://anything.example")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORS_DisabledWhenNoOrigins(t *testing.T) {
	r := newCORSTestRouter(nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://fmsg.io")
	r.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}
