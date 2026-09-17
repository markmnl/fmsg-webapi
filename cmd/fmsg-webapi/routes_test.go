package main

import (
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/internal/handlers"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// TestDelegatedOAuthRouteCoverage keeps the route table and the delegated
// OAuth scope table in step: every bearer-authenticated route is either open
// to delegated tokens under a scope or listed here as deliberately closed.
func TestDelegatedOAuthRouteCoverage(t *testing.T) {
	closed := map[string]bool{
		"GET /fmsg/sub-accounts":                    true,
		"POST /fmsg/sub-accounts":                   true,
		"GET /fmsg/sub-accounts/:agent":             true,
		"PATCH /fmsg/sub-accounts/:agent":           true,
		"POST /fmsg/sub-accounts/:agent/rotate-key": true,
		"DELETE /fmsg/sub-accounts/:agent":          true,
		"POST /fmsg/push/subscribe":                 true,
		"DELETE /fmsg/push/subscribe":               true,
	}
	// POST /fmsg/token authenticates with an API key, not a bearer token.
	unauthenticated := map[string]bool{"POST /fmsg/token": true}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	msgs := handlers.NewMessageHandler(nil, "", 0, 0, 0, nil, "", "")
	registerRoutes(router, func(c *gin.Context) {}, msgs,
		handlers.NewAttachmentHandler(nil, "", 0, 0),
		handlers.NewTokenHandler(nil, nil, ""),
		handlers.NewSubAccountHandler(nil, ""),
		handlers.NewPushHandler(nil, msgs, "", "", "", ""),
		handlers.NewWSHandler(nil, handlers.NewHub(msgs), nil))

	seen := map[string]bool{}
	for _, route := range router.Routes() {
		key := route.Method + " " + route.Path
		seen[key] = true
		_, open := middleware.OAuthRouteScope(route.Method, route.Path)
		switch {
		case unauthenticated[key]:
		case open && closed[key]:
			t.Errorf("%s is both open to and closed to delegated OAuth tokens", key)
		case !open && !closed[key]:
			t.Errorf("%s is not classified for delegated OAuth tokens: add it to the scope table or to the closed list", key)
		}
	}
	for key := range closed {
		if !seen[key] {
			t.Errorf("closed route %s is no longer registered", key)
		}
	}
}
