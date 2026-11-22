package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/internal/handlers"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// TestSearchNotImplemented: GET /fmsg/search is routed to the search handler,
// not taken as GET /fmsg/:id, and answers 501 to an authenticated caller and
// 401 otherwise.
func TestSearchNotImplemented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	msgs := handlers.NewMessageHandler(nil, "", 0, 0, 0, nil, "", "")
	identity := func(c *gin.Context) {
		if id := c.GetHeader("X-Test-Identity"); id != "" {
			c.Set(middleware.IdentityKey, id)
		}
	}
	registerRoutes(router, identity, msgs,
		handlers.NewAttachmentHandler(nil, "", 0, 0),
		handlers.NewTokenHandler(nil, nil, ""),
		handlers.NewSubAccountHandler(nil, ""),
		handlers.NewPushHandler(nil, msgs, "", "", "", ""),
		handlers.NewWSHandler(nil, handlers.NewHub(msgs), nil))

	for _, tc := range []struct {
		identity string
		want     int
	}{
		{"@alice@example.com", http.StatusNotImplemented},
		{"", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, "/fmsg/search?q=hello&limit=1", nil)
		if tc.identity != "" {
			req.Header.Set("X-Test-Identity", tc.identity)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("identity %q: status %d, want %d (body %s)", tc.identity, rec.Code, tc.want, rec.Body.String())
		}
	}
}
