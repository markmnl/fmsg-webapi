package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupRateLimitRouter(rps float64, burst int) *gin.Engine {
	r := gin.New()
	r.Use(NewRateLimiter(context.Background(), rps, burst))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestRateLimiterAllowsUnderLimit(t *testing.T) {
	router := setupRateLimitRouter(10, 5)

	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}
}

func TestRateLimiterBlocksExcessBurst(t *testing.T) {
	router := setupRateLimitRouter(1, 3) // 1 rps, burst of 3

	// First 3 requests should succeed (burst).
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "1.2.3.4:1234"
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, w.Code)
		}
	}

	// Next request should be rate-limited.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	router.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["error"] != "rate limit exceeded" {
		t.Fatalf("unexpected error message: %s", body["error"])
	}
}

func TestRateLimiterTracksIPsIndependently(t *testing.T) {
	router := setupRateLimitRouter(1, 1) // 1 rps, burst of 1

	// Exhaust IP A.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("IP A first request: expected 200, got %d", w.Code)
	}

	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1:1000"
	router.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("IP A second request: expected 429, got %d", w.Code)
	}

	// IP B should still be allowed.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.2:2000"
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("IP B first request: expected 200, got %d", w.Code)
	}
}
