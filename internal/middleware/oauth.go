package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Scopes understood on delegated OAuth tokens. They gate which routes a
// delegated token may call; message permissions, quotas and acceptance checks
// still apply to every request exactly as they do for owner sessions.
const (
	ScopeRead  = "fmsg:read"
	ScopeWrite = "fmsg:write"
)

// IdentitiesClaim optionally lists the addresses, besides the token's own
// address, that a delegated token may select with X-FMSG-Act-As. Listing an
// address never grants it: the owner/sub-account relationship is still checked.
const IdentitiesClaim = "fmsg_identities"

// oauthRouteScopes is the complete set of routes available to delegated OAuth
// tokens, keyed by method and Gin route pattern. Any route absent from this
// table is denied to them, so new routes are closed to delegated tokens until
// listed here. Sub-account (API key and grant) administration and push
// subscriptions are deliberately absent.
var oauthRouteScopes = map[string]string{
	"GET /fmsg":                         ScopeRead,
	"GET /fmsg/sent":                    ScopeRead,
	"GET /fmsg/:id":                     ScopeRead,
	"GET /fmsg/:id/data":                ScopeRead,
	"GET /fmsg/:id/thread":              ScopeRead,
	"GET /fmsg/:id/thread/messages":     ScopeRead,
	"GET /fmsg/:id/attach/:filename":    ScopeRead,
	"GET /fmsg/ws":                      ScopeRead,
	"POST /fmsg":                        ScopeWrite,
	"PUT /fmsg/:id":                     ScopeWrite,
	"DELETE /fmsg/:id":                  ScopeWrite,
	"POST /fmsg/:id/send":               ScopeWrite,
	"POST /fmsg/:id/read":               ScopeWrite,
	"POST /fmsg/:id/add-to":             ScopeWrite,
	"POST /fmsg/:id/react":              ScopeWrite,
	"POST /fmsg/:id/attach":             ScopeWrite,
	"DELETE /fmsg/:id/attach/:filename": ScopeWrite,
}

// OAuthRouteScope returns the scope a delegated OAuth token needs for a route,
// and false when the route is closed to delegated tokens.
func OAuthRouteScope(method, routePattern string) (string, bool) {
	scope, ok := oauthRouteScopes[method+" "+routePattern]
	return scope, ok
}

var errMalformedScope = errors.New("malformed scope claim")

// parseScopes reads the scope claim of a delegated token: the space-delimited
// `scope` string of RFC 9068, or an array of strings under `scope` or `scp`.
// Any other shape is an error; callers must deny rather than assume privileges.
func parseScopes(claims jwt.MapClaims) (map[string]struct{}, error) {
	raw, ok := claims["scope"]
	if !ok {
		raw, ok = claims["scp"]
	}
	if !ok {
		return nil, errMalformedScope
	}
	var items []string
	switch v := raw.(type) {
	case string:
		items = strings.Fields(v)
	case []any:
		for _, e := range v {
			s, ok := e.(string)
			if !ok || s == "" || strings.ContainsAny(s, " \t\r\n") {
				return nil, errMalformedScope
			}
			items = append(items, s)
		}
	default:
		return nil, errMalformedScope
	}
	if len(items) == 0 {
		return nil, errMalformedScope
	}
	scopes := make(map[string]struct{}, len(items))
	for _, s := range items {
		scopes[s] = struct{}{}
	}
	return scopes, nil
}

// delegatedIdentities reads the optional IdentitiesClaim. A missing claim means
// no additional identities; a malformed one is an error.
func delegatedIdentities(claims jwt.MapClaims) ([]string, error) {
	raw, ok := claims[IdentitiesClaim]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("malformed identities claim")
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok || !IsValidAddr(s) {
			return nil, errors.New("malformed identities claim")
		}
		out = append(out, s)
	}
	return out, nil
}

func audienceContains(aud jwt.ClaimStrings, want string) bool {
	for _, a := range aud {
		if a == want {
			return true
		}
	}
	return false
}

// AllowsRoute reports whether the authenticated token may call the route.
// Owner-session and API-key tokens are not scope-restricted here. Delegated
// OAuth tokens need the scope listed for the route and are denied every route
// that is not listed.
func (r authResult) AllowsRoute(method, routePattern string) (ok bool, msg string) {
	if r.AuthType != AuthTypeOAuth {
		return true, ""
	}
	need, listed := OAuthRouteScope(method, routePattern)
	if !listed {
		return false, "this operation is not available to delegated OAuth tokens"
	}
	if _, has := r.Scopes[need]; !has {
		return false, "token lacks required scope " + need
	}
	return true, ""
}

// CheckScope aborts the request with 403 insufficient_scope when the token may
// not call the matched route. It returns true when the request may proceed.
func CheckScope(c *gin.Context, res authResult) bool {
	ok, msg := res.AllowsRoute(c.Request.Method, c.FullPath())
	if ok {
		return true
	}
	c.Header("WWW-Authenticate", `Bearer error="insufficient_scope"`)
	respondAuth(c, http.StatusForbidden, msg)
	return false
}
