package middleware

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
)

const testOAuthAudience = "https://api.example.test/fmsg"

// adminRoutes are the routes that must stay closed to delegated OAuth tokens.
var adminRoutes = [][2]string{
	{http.MethodGet, "/fmsg/sub-accounts"},
	{http.MethodPost, "/fmsg/sub-accounts"},
	{http.MethodGet, "/fmsg/sub-accounts/:agent"},
	{http.MethodPatch, "/fmsg/sub-accounts/:agent"},
	{http.MethodPost, "/fmsg/sub-accounts/:agent/rotate-key"},
	{http.MethodDelete, "/fmsg/sub-accounts/:agent"},
	{http.MethodPost, "/fmsg/push/subscribe"},
	{http.MethodDelete, "/fmsg/push/subscribe"},
}

type whoami struct {
	Identity string `json:"identity"`
	Owner    string `json:"owner"`
	AuthType string `json:"auth_type"`
}

// oauthEngine serves every delegated and administrative route pattern behind
// the real authentication middleware, answering with the resolved identity.
func oauthEngine(t *testing.T, apiKeys APIKeyChecker) (*gin.Engine, ed25519.PrivateKey) {
	t.Helper()
	srv := fmsgIDServer(t, http.StatusOK, true)
	t.Cleanup(srv.Close)
	priv, jwks := newEdDSAFixture(t)
	cfg := providerConfig(srv.URL, jwks)
	cfg.OAuthAudience = testOAuthAudience
	if apiKeys != nil {
		apiPub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cfg.APIPublicKey = apiPub
		cfg.APIKeys = apiKeys
	}
	mw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := gin.New()
	r.Use(mw)
	echo := func(c *gin.Context) {
		c.JSON(http.StatusOK, whoami{GetIdentity(c), GetOwnerIdentity(c), GetAuthType(c)})
	}
	for key := range oauthRouteScopes {
		method, pattern, _ := strings.Cut(key, " ")
		r.Handle(method, pattern, echo)
	}
	for _, route := range adminRoutes {
		r.Handle(route[0], route[1], echo)
	}
	return r, priv
}

func delegatedClaims(addr string, scope any) jwt.MapClaims {
	claims := providerClaims(addr)
	claims["aud"] = testOAuthAudience
	claims["client_id"] = "https://client.example.test/metadata.json"
	claims["act"] = map[string]any{"sub": "mcp-server"}
	if scope != nil {
		claims["scope"] = scope
	}
	return claims
}

func call(t *testing.T, r *gin.Engine, method, path, token, actAs string) (*httptest.ResponseRecorder, whoami) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Authorization", "Bearer "+token)
	if actAs != "" {
		req.Header.Set("X-FMSG-Act-As", actAs)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var who whoami
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	return w, who
}

// concrete turns a route pattern into a request path.
func concrete(pattern string) string {
	return strings.NewReplacer(":id", "42", ":filename", "a.txt", ":agent", "bot").Replace(pattern)
}

func TestOAuth_MessagingWorksForConsentedIdentity(t *testing.T) {
	r, priv := oauthEngine(t, nil)
	tok := signEdDSA(t, priv, "prod-1", delegatedClaims("@alice@example.com", ScopeRead+" "+ScopeWrite))
	for key := range oauthRouteScopes {
		method, pattern, _ := strings.Cut(key, " ")
		w, who := call(t, r, method, concrete(pattern), tok, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d body=%s", key, w.Code, w.Body.String())
		}
		if who.Identity != "@alice@example.com" || who.Owner != "@alice@example.com" || who.AuthType != AuthTypeOAuth {
			t.Fatalf("%s: unexpected identity %+v", key, who)
		}
	}
}

func TestOAuth_ScopeArrayForms(t *testing.T) {
	r, priv := oauthEngine(t, nil)
	claims := delegatedClaims("@alice@example.com", nil)
	claims["scp"] = []string{ScopeRead}
	tok := signEdDSA(t, priv, "prod-1", claims)
	if w, _ := call(t, r, http.MethodGet, "/fmsg", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("scp array: expected 200, got %d", w.Code)
	}
}

func TestOAuth_ScopeLimitsRoutes(t *testing.T) {
	r, priv := oauthEngine(t, nil)
	tok := signEdDSA(t, priv, "prod-1", delegatedClaims("@alice@example.com", ScopeRead))
	if w, _ := call(t, r, http.MethodGet, "/fmsg", tok, ""); w.Code != http.StatusOK {
		t.Fatalf("read: expected 200, got %d", w.Code)
	}
	w, _ := call(t, r, http.MethodPost, "/fmsg/42/send", tok, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("send with read scope: expected 403, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope"` {
		t.Fatalf("WWW-Authenticate=%q", got)
	}
}

func TestOAuth_AdministrationDenied(t *testing.T) {
	r, priv := oauthEngine(t, fakeAPIKeys{})
	// Even a token claiming every scope, including invented ones, is refused.
	tok := signEdDSA(t, priv, "prod-1", delegatedClaims("@alice@example.com", ScopeRead+" "+ScopeWrite+" fmsg:admin admin *"))
	for _, route := range adminRoutes {
		if w, _ := call(t, r, route[0], concrete(route[1]), tok, ""); w.Code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403, got %d", route[0], route[1], w.Code)
		}
	}
}

func TestOAuth_MissingOrMalformedScopeNeverGrantsOwner(t *testing.T) {
	r, priv := oauthEngine(t, fakeAPIKeys{})
	cases := map[string]func(jwt.MapClaims){
		"missing":        func(c jwt.MapClaims) {},
		"empty string":   func(c jwt.MapClaims) { c["scope"] = "" },
		"blank string":   func(c jwt.MapClaims) { c["scope"] = "   " },
		"number":         func(c jwt.MapClaims) { c["scope"] = 7 },
		"object":         func(c jwt.MapClaims) { c["scope"] = map[string]any{"fmsg:read": true} },
		"empty array":    func(c jwt.MapClaims) { c["scope"] = []string{} },
		"mixed array":    func(c jwt.MapClaims) { c["scp"] = []any{ScopeRead, 1} },
		"spaced element": func(c jwt.MapClaims) { c["scp"] = []any{"fmsg:read fmsg:write"} },
		"null":           func(c jwt.MapClaims) { c["scope"] = nil },
	}
	for name, mutate := range cases {
		claims := delegatedClaims("@alice@example.com", nil)
		mutate(claims)
		tok := signEdDSA(t, priv, "prod-1", claims)
		for _, route := range [][2]string{{http.MethodGet, "/fmsg"}, {http.MethodPost, "/fmsg/sub-accounts"}} {
			if w, _ := call(t, r, route[0], route[1], tok, ""); w.Code != http.StatusForbidden {
				t.Fatalf("%s %s %s: expected 403, got %d body=%s", name, route[0], route[1], w.Code, w.Body.String())
			}
		}
	}
}

func TestOAuth_AudienceEnforced(t *testing.T) {
	r, priv := oauthEngine(t, nil)
	cases := map[string]func(jwt.MapClaims){
		"both audiences":          func(c jwt.MapClaims) { c["aud"] = []string{testAudience, testOAuthAudience} },
		"other resource audience": func(c jwt.MapClaims) { c["aud"] = "https://mcp.example.test/mcp" },
		"no audience":             func(c jwt.MapClaims) { delete(c, "aud") },
		"actor on owner audience": func(c jwt.MapClaims) { c["aud"] = testAudience },
	}
	for name, mutate := range cases {
		claims := delegatedClaims("@alice@example.com", ScopeRead+" "+ScopeWrite)
		mutate(claims)
		tok := signEdDSA(t, priv, "prod-1", claims)
		if w, _ := call(t, r, http.MethodGet, "/fmsg", tok, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d body=%s", name, w.Code, w.Body.String())
		}
	}
}

func TestOAuth_DelegatedAudienceRejectedUntilEnabled(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(providerConfig(srv.URL, jwks)) // no OAuthAudience
	if err != nil {
		t.Fatal(err)
	}
	tok := signEdDSA(t, priv, "prod-1", delegatedClaims("@alice@example.com", ScopeRead))
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestOAuth_ActAsCannotBroadenGrant(t *testing.T) {
	const sub = "@alice_bot@example.com"
	// The owner does own the sub-account, yet the grant does not record it.
	r, priv := oauthEngine(t, fakeAPIKeys{})
	tok := signEdDSA(t, priv, "prod-1", delegatedClaims("@alice@example.com", ScopeRead+" "+ScopeWrite))
	if w, _ := call(t, r, http.MethodGet, "/fmsg", tok, sub); w.Code != http.StatusForbidden {
		t.Fatalf("unlisted act-as: expected 403, got %d", w.Code)
	}

	for name, identities := range map[string]any{"string": sub, "invalid entry": []any{"bot"}, "mixed": []any{sub, 3}} {
		claims := delegatedClaims("@alice@example.com", ScopeRead)
		claims[IdentitiesClaim] = identities
		bad := signEdDSA(t, priv, "prod-1", claims)
		if w, _ := call(t, r, http.MethodGet, "/fmsg", bad, sub); w.Code != http.StatusForbidden {
			t.Fatalf("malformed identities (%s): expected 403, got %d", name, w.Code)
		}
	}

	claims := delegatedClaims("@alice@example.com", ScopeRead+" "+ScopeWrite)
	claims[IdentitiesClaim] = []string{sub}
	listed := signEdDSA(t, priv, "prod-1", claims)
	w, who := call(t, r, http.MethodGet, "/fmsg", listed, sub)
	if w.Code != http.StatusOK || who.Identity != sub || who.Owner != "@alice@example.com" || who.AuthType != AuthTypeOAuth {
		t.Fatalf("listed act-as: code=%d who=%+v", w.Code, who)
	}
	// Switching identity does not reopen administration.
	for _, route := range adminRoutes {
		if w, _ := call(t, r, route[0], concrete(route[1]), listed, sub); w.Code != http.StatusForbidden {
			t.Fatalf("act-as %s %s: expected 403, got %d", route[0], route[1], w.Code)
		}
	}

	// Listing an identity the owner does not own grants nothing.
	r, priv = oauthEngine(t, fakeAPIKeys{actErr: apiauth.ErrNotFound})
	listed = signEdDSA(t, priv, "prod-1", claims)
	if w, _ := call(t, r, http.MethodGet, "/fmsg", listed, sub); w.Code != http.StatusForbidden {
		t.Fatalf("listed but unowned: expected 403, got %d", w.Code)
	}
}

func TestOAuth_OwnerSessionsUnchanged(t *testing.T) {
	r, priv := oauthEngine(t, fakeAPIKeys{})
	claims := providerClaims("@alice@example.com")
	// Some providers put a scope claim on ordinary sign-in tokens.
	claims["scope"] = "openid profile"
	tok := signEdDSA(t, priv, "prod-1", claims)
	for _, route := range adminRoutes {
		w, who := call(t, r, route[0], concrete(route[1]), tok, "")
		if w.Code != http.StatusOK || who.AuthType != AuthTypeIdP {
			t.Fatalf("owner %s %s: code=%d who=%+v", route[0], route[1], w.Code, who)
		}
	}
	w, who := call(t, r, http.MethodPost, "/fmsg", tok, "@alice_bot@example.com")
	if w.Code != http.StatusOK || who.Identity != "@alice_bot@example.com" || who.AuthType != AuthTypeIdP {
		t.Fatalf("owner act-as: code=%d who=%+v", w.Code, who)
	}
}

func TestOAuth_ConfigValidation(t *testing.T) {
	_, jwks := newEdDSAFixture(t)
	cfg := providerConfig("http://127.0.0.1:1", jwks)
	cfg.OAuthAudience = cfg.Audience
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error when OAuthAudience equals Audience")
	}
	cfg.Audience = ""
	cfg.OAuthAudience = testOAuthAudience
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error when OAuthAudience is set without Audience")
	}
}
