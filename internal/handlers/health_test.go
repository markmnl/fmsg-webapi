package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(ctx context.Context) error { return f.err }

func TestHealthz(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name string
		err  error
		code int
		body string
	}{
		{"database up", nil, http.StatusOK, "ok"},
		{"database down", errors.New("connection refused"), http.StatusServiceUnavailable, "database unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/healthz", Healthz(fakePinger{tc.err}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if w.Code != tc.code || w.Body.String() != tc.body {
				t.Fatalf("got %d %q, want %d %q", w.Code, w.Body.String(), tc.code, tc.body)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}
