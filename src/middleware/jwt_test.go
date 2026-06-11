package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Provider values used by the RS256 fixtures.
const (
	testIssuer       = "https://issuer.example.test/"
	testAudience     = "fmsg-web-client"
	testAddressClaim = "fmsg_address"
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

// fakeJWKS returns a jwt.Keyfunc that yields a fixed RSA public key for a
// single known kid.
func fakeJWKS(kid string, pub *rsa.PublicKey) jwt.Keyfunc {
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

func signRS256(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
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

// rs256Claims returns provider-token-shaped claims carrying the given fmsg
// address in the configured address claim.
func rs256Claims(addr string) jwt.MapClaims {
	claims := jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": "provider|abc123",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	if addr != "" {
		claims[testAddressClaim] = addr
	}
	return claims
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

func newRS256Fixture(t *testing.T) (priv *rsa.PrivateKey, jwks jwt.Keyfunc) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return priv, fakeJWKS("prod-1", &priv.PublicKey)
}

func rs256Config(idURL string, jwks jwt.Keyfunc) Config {
	return Config{
		Mode:         ModeRS256,
		JWKS:         jwks,
		Issuer:       testIssuer,
		Audience:     testAudience,
		AddressClaim: testAddressClaim,
		IDURL:        idURL,
	}
}

func TestRS256Mode_Happy(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRS256Mode_IdentityIsAddressClaim(t *testing.T) {
	const addr = "@claim@example.com"
	fmsgIDCache.Delete(addr)
	defer fmsgIDCache.Delete(addr)

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/fmsgid/"+addr {
			http.Error(w, "wrong address", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": true})
	}))
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	v, err := NewVerifier(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	tok := signRS256(t, priv, "prod-1", rs256Claims(addr))
	gotAddr, status, _ := v.Authenticate(tok)
	if status != http.StatusOK || gotAddr != addr {
		t.Fatalf("got addr=%q status=%d, want %s/200", gotAddr, status, addr)
	}
	if hits != 1 {
		t.Fatalf("fmsgid hits = %d, want 1", hits)
	}
}

func TestRS256Mode_MissingAddressClaim(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	// A valid ID token whose identity has no fmsg account yet.
	tok := signRS256(t, priv, "prod-1", rs256Claims(""))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRS256Mode_MalformedAddressClaim(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("not-an-address"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_WrongIssuer(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	claims := rs256Claims("@alice@example.com")
	claims["iss"] = "https://evil.example.com/"
	tok := signRS256(t, priv, "prod-1", claims)
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_WrongAudience(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}

	// Token minted for a different configured application or API.
	claims := rs256Claims("@alice@example.com")
	claims["aud"] = "SomeOtherClientID"
	tok := signRS256(t, priv, "prod-1", claims)
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong aud: expected 401, got %d", w.Code)
	}

	// Token with no audience at all.
	claims = rs256Claims("@alice@example.com")
	delete(claims, "aud")
	tok = signRS256(t, priv, "prod-1", claims)
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("missing aud: expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_UnknownKID(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "rotated-key", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_AlgDowngrade(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	_, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	// Sign with HS256 - must be rejected by an RS256-only middleware.
	tok := signHS256(t, []byte("anything"), rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_Expired(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	claims := rs256Claims("@alice@example.com")
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	tok := signRS256(t, priv, "prod-1", claims)
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_Reuse(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))

	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("first call expected 200, got %d", w.Code)
	}
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusOK {
		t.Fatalf("reuse expected 200, got %d", w.Code)
	}
}

func TestRS256Mode_ConfigValidation(t *testing.T) {
	_, jwks := newRS256Fixture(t)

	if _, err := NewVerifier(Config{Mode: ModeRS256, Issuer: testIssuer, Audience: testAudience, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing JWKS: expected error")
	}
	if _, err := NewVerifier(Config{Mode: ModeRS256, JWKS: jwks, Audience: testAudience, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing Issuer: expected error")
	}
	if _, err := NewVerifier(Config{Mode: ModeRS256, JWKS: jwks, Issuer: testIssuer, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing Audience: expected error")
	}
	if _, err := NewVerifier(Config{Mode: ModeRS256, JWKS: jwks, Issuer: testIssuer, Audience: testAudience}); err == nil {
		t.Error("missing AddressClaim: expected error")
	}
}

func TestRS256Mode_FmsgIDNotFound(t *testing.T) {
	fmsgIDCache.Delete("@alice@example.com")
	defer fmsgIDCache.Delete("@alice@example.com")

	srv := fmsgIDServer(t, http.StatusNotFound, false)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRS256Mode_FmsgIDNotAccepting(t *testing.T) {
	fmsgIDCache.Delete("@alice@example.com")
	defer fmsgIDCache.Delete("@alice@example.com")

	srv := fmsgIDServer(t, http.StatusOK, false)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestVerifier_Authenticate(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	secret := []byte("dev-secret")
	v, err := NewVerifier(Config{Mode: ModeHMAC, HMACKey: secret, IDURL: srv.URL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// Valid token is accepted and yields the sub address.
	tok := signHS256(t, secret, jwt.MapClaims{
		"sub": "@alice@example.com",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	addr, status, _ := v.Authenticate(tok)
	if status != http.StatusOK || addr != "@alice@example.com" {
		t.Fatalf("valid token: got addr=%q status=%d, want @alice@example.com/200", addr, status)
	}

	// Token signed with the wrong secret is rejected.
	bad := signHS256(t, []byte("wrong-secret"), jwt.MapClaims{
		"sub": "@alice@example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, status, _ := v.Authenticate(bad); status != http.StatusUnauthorized {
		t.Fatalf("bad signature: expected 401, got %d", status)
	}

	// Token with a malformed sub is rejected.
	noaddr := signHS256(t, secret, jwt.MapClaims{
		"sub": "not-an-address",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, status, _ := v.Authenticate(noaddr); status != http.StatusUnauthorized {
		t.Fatalf("invalid addr: expected 401, got %d", status)
	}
}

func TestRS256Mode_FmsgIDUnavailable(t *testing.T) {
	fmsgIDCache.Delete("@alice@example.com")
	srv := fmsgIDServer(t, http.StatusInternalServerError, false)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
