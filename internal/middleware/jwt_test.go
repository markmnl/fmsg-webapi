package middleware

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
)

const (
	testIssuer       = "https://issuer.example.test/"
	testAudience     = "fmsg-web-client"
	testAddressClaim = "fmsg_address"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type fakeAPIKeys struct {
	tokenErr error
	actErr   error
}

func (f fakeAPIKeys) ValidateToken(_ context.Context, keyID, ownerAddr, subAddr, remoteAddr string) error {
	if keyID == "" || ownerAddr == "" || subAddr == "" || remoteAddr == "" {
		return errors.New("missing token validation input")
	}
	return f.tokenErr
}

func (f fakeAPIKeys) ValidateActAs(_ context.Context, ownerAddr, subAddr string) error {
	if ownerAddr == "" || subAddr == "" {
		return errors.New("missing act-as input")
	}
	return f.actErr
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

func fakeJWKS(kid string, pub *rsa.PublicKey) jwt.Keyfunc {
	return func(t *jwt.Token) (interface{}, error) {
		k, _ := t.Header["kid"].(string)
		if k != kid {
			return nil, jwt.ErrTokenSignatureInvalid
		}
		return pub, nil
	}
}

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

func runMiddleware(t *testing.T, mw gin.HandlerFunc, token string, actAs string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/fmsg", nil)
	c.Request.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		c.Request.Header.Set("Authorization", "Bearer "+token)
	}
	if actAs != "" {
		c.Request.Header.Set("X-FMSG-Act-As", actAs)
	}
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
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRS256Mode_ActAsSubAccount(t *testing.T) {
	fmsgIDCache.Delete("@alice@example.com")
	fmsgIDCache.Delete("@alice_bot@example.com")
	defer fmsgIDCache.Delete("@alice@example.com")
	defer fmsgIDCache.Delete("@alice_bot@example.com")

	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	apiPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mw, err := New(Config{
		JWKS:         jwks,
		Issuer:       testIssuer,
		Audience:     testAudience,
		AddressClaim: testAddressClaim,
		IDURL:        srv.URL,
		APIPublicKey: apiPub,
		APIKeys:      fakeAPIKeys{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/fmsg", nil)
	c.Request.RemoteAddr = "127.0.0.1:12345"
	c.Request.Header.Set("Authorization", "Bearer "+tok)
	c.Request.Header.Set("X-FMSG-Act-As", "@alice_bot@example.com")
	mw(c)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if got := GetIdentity(c); got != "@alice_bot@example.com" {
		t.Fatalf("identity=%q", got)
	}
	if got := GetOwnerIdentity(c); got != "@alice@example.com" {
		t.Fatalf("owner=%q", got)
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
	tok := signRS256(t, priv, "prod-1", rs256Claims(""))
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRS256Mode_WrongIssuerAudienceAndExpired(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	priv, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}

	claims := rs256Claims("@alice@example.com")
	claims["iss"] = "https://evil.example.com/"
	if w := runMiddleware(t, mw, signRS256(t, priv, "prod-1", claims), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong issuer expected 401, got %d", w.Code)
	}

	claims = rs256Claims("@alice@example.com")
	claims["aud"] = "other"
	if w := runMiddleware(t, mw, signRS256(t, priv, "prod-1", claims), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong audience expected 401, got %d", w.Code)
	}

	claims = rs256Claims("@alice@example.com")
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if w := runMiddleware(t, mw, signRS256(t, priv, "prod-1", claims), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_RejectsHMACAlg(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	_, jwks := newRS256Fixture(t)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signHS256(t, []byte("anything"), rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRS256Mode_ConfigValidation(t *testing.T) {
	_, jwks := newRS256Fixture(t)

	if _, err := NewVerifier(Config{Issuer: testIssuer, Audience: testAudience, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing auth modes: expected error")
	}
	if _, err := NewVerifier(Config{JWKS: jwks, Audience: testAudience, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing Issuer: expected error")
	}
	if _, err := NewVerifier(Config{JWKS: jwks, Issuer: testIssuer, AddressClaim: testAddressClaim}); err == nil {
		t.Error("missing Audience: expected error")
	}
	if _, err := NewVerifier(Config{JWKS: jwks, Issuer: testIssuer, Audience: testAudience}); err == nil {
		t.Error("missing AddressClaim: expected error")
	}
}

func TestRS256Mode_FmsgIDFailures(t *testing.T) {
	priv, jwks := newRS256Fixture(t)

	fmsgIDCache.Delete("@alice@example.com")
	srv := fmsgIDServer(t, http.StatusNotFound, false)
	mw, err := New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	tok := signRS256(t, priv, "prod-1", rs256Claims("@alice@example.com"))
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("not found expected 400, got %d", w.Code)
	}
	srv.Close()

	fmsgIDCache.Delete("@alice@example.com")
	srv = fmsgIDServer(t, http.StatusOK, false)
	defer srv.Close()
	mw, err = New(rs256Config(srv.URL, jwks))
	if err != nil {
		t.Fatal(err)
	}
	if w := runMiddleware(t, mw, tok, ""); w.Code != http.StatusForbidden {
		t.Fatalf("not accepting expected 403, got %d", w.Code)
	}
}

func TestAPITokenMode_Happy(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := apiauth.NewTokenIssuer(priv, apiauth.DefaultTokenIssuer, apiauth.DefaultTokenAudience, time.Hour)
	token, _, err := issuer.Mint("@alice@example.com", "@alice_bot@example.com", "kid1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	mw, err := New(Config{
		APIPublicKey: issuer.PublicKey(),
		APIIssuer:    issuer.Issuer(),
		APIAudience:  issuer.Audience(),
		APIKeys:      fakeAPIKeys{},
		IDURL:        srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := runMiddleware(t, mw, token, ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAPITokenMode_RejectsActAsAndRevokedKey(t *testing.T) {
	srv := fmsgIDServer(t, http.StatusOK, true)
	defer srv.Close()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := apiauth.NewTokenIssuer(priv, "", "", time.Hour)
	token, _, err := issuer.Mint("@alice@example.com", "@alice_bot@example.com", "kid1", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	mw, err := New(Config{
		APIPublicKey: issuer.PublicKey(),
		APIKeys:      fakeAPIKeys{},
		IDURL:        srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := runMiddleware(t, mw, token, "@alice_other@example.com"); w.Code != http.StatusForbidden {
		t.Fatalf("act-as expected 403, got %d", w.Code)
	}

	mw, err = New(Config{
		APIPublicKey: issuer.PublicKey(),
		APIKeys:      fakeAPIKeys{tokenErr: apiauth.ErrKeyRevoked},
		IDURL:        srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := runMiddleware(t, mw, token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked expected 401, got %d", w.Code)
	}
}

func TestAPITokenMode_ConfigValidation(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewVerifier(Config{APIPublicKey: pub}); err == nil {
		t.Fatal("missing API key checker: expected error")
	}
}
