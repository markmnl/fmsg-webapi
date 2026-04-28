package middleware

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
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestIsValidAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"@alice@example.com", true},
		{"@a@b", true},
		{"@user@domain.org", true},
		{"alice@example.com", false},
		{"@nodomain", false},
		{"@", false},
		{"ab", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			if got := IsValidAddr(tt.addr); got != tt.want {
				t.Errorf("isValidAddr(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

// fakeJWKS returns a jwt.Keyfunc that yields a fixed Ed25519 public key for
// a single known kid.
func fakeJWKS(kid string, pub ed25519.PublicKey) jwt.Keyfunc {
	return func(t *jwt.Token) (interface{}, error) {
		k, _ := t.Header["kid"].(string)
		if k != kid {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return pub, nil
	}
}

// fmsgIDServer returns an httptest server emulating fmsgid responses.
func fmsgIDServer(t *testing.T, status int, accepting bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": accepting})
	}))
}

// runMiddleware executes the middleware against a synthetic request bearing
// the given token, returning the recorded response.
func runMiddleware(t *testing.T, mw gin.HandlerFunc, token string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/fmsg", nil)
	if token != "" {
		c.Request.Header.Set("Authorization", "Bearer "+token)
	}
	called := false
	c.Set("__test_next__", &called)
	mw(c)
	return w
}

func signEdDSA(t *testing.T, priv ed25519.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func signHS256(t *testing.T, secret []byte, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(secret)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func TestHMACMode_Happy(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()

	secret := []byte("dev-secret")
	mw, err := New(Config{Mode: ModeHMAC, HMACKey: secret, IDURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := signHS256(t, secret, jwt.MapClaims{
		"sub": "@alice@example.com",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	w := runMiddleware(t, mw, tok)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHMACMode_ClockSkewLeeway(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	secret := []byte("dev-secret")
	mw, err := New(Config{Mode: ModeHMAC, HMACKey: secret, IDURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// iat/nbf within leeway is accepted.
	now := time.Now()
	tok := signHS256(t, secret, jwt.MapClaims{
		"sub": "@alice@example.com",
		"iat": now.Add(DefaultClockSkew - time.Second).Unix(),
		"nbf": now.Add(DefaultClockSkew - time.Second).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("within-skew token should be accepted, got %d", w.Code)
	}

	// Beyond leeway is rejected.
	tok = signHS256(t, secret, jwt.MapClaims{
		"sub": "@alice@example.com",
		"nbf": now.Add(DefaultClockSkew + 5*time.Second).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("out-of-skew token should be rejected, got %d", w.Code)
	}
}

func TestHMACMode_MissingHeader(t *testing.T) {
	mw, err := New(Config{Mode: ModeHMAC, HMACKey: []byte("k"), IDURL: "http://127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	if w := runMiddleware(t, mw, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func newEdDSAFixture(t *testing.T) (priv ed25519.PrivateKey, jwks jwt.Keyfunc) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return priv, fakeJWKS("prod-1", pub)
}

func eddsaConfig(idURL string, jwks jwt.Keyfunc) Config {
	return Config{
		Mode:   ModeEdDSA,
		JWKS:   jwks,
		Issuer: "https://idp.fmsg.io",
		IDURL:  idURL,
	}
}

func TestEdDSAMode_Happy(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := signEdDSA(t, priv, "prod-1", jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"iat": time.Now().Unix(),
		"nbf": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "11111111-1111-1111-1111-111111111111",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestEdDSAMode_WrongIssuer(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signEdDSA(t, priv, "prod-1", jwt.MapClaims{
		"iss": "https://evil.example.com",
		"sub": "@alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "a",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestEdDSAMode_UnknownKID(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signEdDSA(t, priv, "rotated-key", jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "a",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestEdDSAMode_AlgDowngrade(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	_, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	// Sign with HS256 — must be rejected by an EdDSA-only middleware.
	tok := signHS256(t, []byte("anything"), jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "a",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestEdDSAMode_Expired(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signEdDSA(t, priv, "prod-1", jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"exp": time.Now().Add(-time.Hour).Unix(),
		"jti": "a",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestEdDSAMode_Reuse(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "reuse-me",
	}
	tok := signEdDSA(t, priv, "prod-1", claims)

	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("first call expected 200, got %d", w.Code)
	}
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("reuse expected 200, got %d", w.Code)
	}
}

func TestEdDSAMode_FmsgIDUnavailable(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusInternalServerError, false)
	defer srv.Close()
	priv, jwks := newEdDSAFixture(t)
	mw, err := New(eddsaConfig(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signEdDSA(t, priv, "prod-1", jwt.MapClaims{
		"iss": "https://idp.fmsg.io",
		"sub": "@alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
		"jti": "x",
	})
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
