package handlers

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// A delegated OAuth token needs the read scope to open the WebSocket; the
// refusal happens before the upgrade.
func TestWSConnectRequiresReadScopeForDelegatedTokens(t *testing.T) {
	idSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": true})
	}))
	defer idSrv.Close()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := middleware.NewVerifier(middleware.Config{
		JWKS:          func(*jwt.Token) (any, error) { return pub, nil },
		Issuer:        "https://issuer.example.test/",
		Audience:      "fmsg-web-client",
		OAuthAudience: "https://api.example.test/fmsg",
		AddressClaim:  "sub",
		IDURL:         idSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.GET("/fmsg/ws", NewWSHandler(verifier, NewHub(nil), nil).Connect)

	connect := func(scope string) int {
		tok, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
			"iss": "https://issuer.example.test/", "aud": "https://api.example.test/fmsg",
			"sub": "@alice@example.com", "scope": scope,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
		}).SignedString(priv)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "/fmsg/ws", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	if code := connect(middleware.ScopeWrite); code != http.StatusForbidden {
		t.Fatalf("write-only token: expected 403, got %d", code)
	}
	// With the read scope the request passes authorization and fails only
	// because this plain HTTP request is not a WebSocket upgrade.
	if code := connect(middleware.ScopeRead); code != http.StatusBadRequest {
		t.Fatalf("read token: expected 400 from the upgrader, got %d", code)
	}
}
