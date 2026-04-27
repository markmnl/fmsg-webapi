// Package middleware configures the JWT authentication middleware.
package middleware

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// IdentityKey is the Gin context key under which the authenticated user
// address is stored.
const IdentityKey = "sub"

// DefaultClockSkew is the leeway applied to iat/nbf/exp validation to tolerate
// minor clock differences between services.
const DefaultClockSkew = 10 * time.Second

// Mode selects the JWT verification strategy.
type Mode int

const (
	// ModeHMAC verifies HS256 tokens with a shared symmetric secret.
	// Intended for development and testing.
	ModeHMAC Mode = iota
	// ModeEdDSA verifies EdDSA (Ed25519) tokens whose public keys are
	// served by an external IdP via JWKS.
	ModeEdDSA
)

// Config configures the JWT middleware.
type Config struct {
	// Mode selects HMAC (dev) or EdDSA (prod) verification.
	Mode Mode

	// HMACKey is the symmetric secret bytes (required when Mode == ModeHMAC).
	HMACKey []byte

	// JWKS resolves Ed25519 public keys (typically by token header `kid`).
	// Required when Mode == ModeEdDSA.
	JWKS jwt.Keyfunc

	// Issuer, when non-empty, is required to match the token `iss` claim.
	// Mandatory in EdDSA mode.
	Issuer string

	// Audience, when non-empty, is required to be present in the token
	// `aud` claim. Optional.
	Audience string

	// IDURL is the base URL of the fmsgid identity service.
	IDURL string

	// ClockSkew is the leeway applied to time-based claim validation.
	// Defaults to DefaultClockSkew when zero.
	ClockSkew time.Duration
}

// New constructs the JWT verification middleware.
//
// The returned handler:
//   - extracts a Bearer token from the Authorization header,
//   - parses & verifies the signature according to cfg.Mode,
//   - validates iss/aud/exp/nbf claims,
//   - rejects replays (EdDSA mode only) by tracking jti in-process,
//   - extracts sub as the user address and validates its shape,
//   - calls fmsgid to confirm the user is known and accepting messages,
//   - on success stores the address in the Gin context under IdentityKey.
//
// On failure the response is 400/401/403/503 with a JSON `{"error": "..."}` body.
func New(cfg Config) (gin.HandlerFunc, error) {
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = DefaultClockSkew
	}

	var (
		validMethods []string
		keyFunc      jwt.Keyfunc
	)

	switch cfg.Mode {
	case ModeHMAC:
		if len(cfg.HMACKey) == 0 {
			return nil, errors.New("middleware: HMAC mode requires a non-empty HMACKey")
		}
		validMethods = []string{jwt.SigningMethodHS256.Alg()}
		key := cfg.HMACKey
		keyFunc = func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
			}
			return key, nil
		}
	case ModeEdDSA:
		if cfg.JWKS == nil {
			return nil, errors.New("middleware: EdDSA mode requires a JWKS keyfunc")
		}
		if cfg.Issuer == "" {
			return nil, errors.New("middleware: EdDSA mode requires an Issuer")
		}
		validMethods = []string{jwt.SigningMethodEdDSA.Alg()}
		jwks := cfg.JWKS
		keyFunc = func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
			}
			return jwks(t)
		}
	default:
		return nil, fmt.Errorf("middleware: unknown JWT mode %d", cfg.Mode)
	}

	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods(validMethods),
		jwt.WithLeeway(cfg.ClockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
	}
	if cfg.Issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(cfg.Audience))
	}
	parser := jwt.NewParser(parserOpts...)

	var replay *jtiCache
	if cfg.Mode == ModeEdDSA {
		replay = newJTICache()
	}

	idURL := cfg.IDURL

	return func(c *gin.Context) {
		tokenStr, err := extractBearer(c.GetHeader("Authorization"))
		if err != nil {
			respondAuth(c, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}

		claims := jwt.MapClaims{}
		if _, err := parser.ParseWithClaims(tokenStr, claims, keyFunc); err != nil {
			log.Printf("auth rejected: ip=%s reason=parse_error err=%v", c.ClientIP(), err)
			respondAuth(c, http.StatusUnauthorized, "invalid token")
			return
		}

		addr, _ := claims["sub"].(string)
		if !IsValidAddr(addr) {
			log.Printf("auth rejected: ip=%s reason=invalid_addr sub=%q", c.ClientIP(), addr)
			respondAuth(c, http.StatusUnauthorized, "invalid identity")
			return
		}

		if replay != nil {
			jti, _ := claims["jti"].(string)
			if jti == "" {
				log.Printf("auth rejected: ip=%s addr=%s reason=missing_jti", c.ClientIP(), addr)
				respondAuth(c, http.StatusUnauthorized, "invalid token")
				return
			}
			expTime, err := claims.GetExpirationTime()
			if err != nil || expTime == nil {
				respondAuth(c, http.StatusUnauthorized, "invalid token")
				return
			}
			if replay.Seen(jti, expTime.Time) {
				log.Printf("auth rejected: ip=%s addr=%s reason=jti_replay jti=%s", c.ClientIP(), addr, jti)
				respondAuth(c, http.StatusUnauthorized, "token already used")
				return
			}
		}

		code, accepting, err := checkFmsgID(idURL, addr)
		if err != nil {
			log.Printf("fmsgid check error for %s: %v", addr, err)
			respondAuth(c, http.StatusServiceUnavailable, "identity service unavailable")
			return
		}
		switch {
		case code == http.StatusNotFound:
			log.Printf("auth rejected: ip=%s addr=%s reason=not_found", c.ClientIP(), addr)
			respondAuth(c, http.StatusBadRequest, fmt.Sprintf("User %s not found", addr))
			return
		case code == http.StatusOK && !accepting:
			log.Printf("auth rejected: ip=%s addr=%s reason=not_accepting", c.ClientIP(), addr)
			respondAuth(c, http.StatusForbidden, fmt.Sprintf("User %s not authorised to send new messages", addr))
			return
		case code != http.StatusOK:
			log.Printf("auth rejected: ip=%s addr=%s reason=fmsgid_status=%d", c.ClientIP(), addr, code)
			respondAuth(c, http.StatusServiceUnavailable, "identity service unavailable")
			return
		}

		c.Set(IdentityKey, addr)
		c.Next()
	}, nil
}

// respondAuth aborts the request with a JSON error body.
func respondAuth(c *gin.Context, code int, message string) {
	log.Printf("auth failure: ip=%s code=%d message=%s", c.ClientIP(), code, message)
	c.AbortWithStatusJSON(code, gin.H{"error": message})
}

// extractBearer returns the token portion of an Authorization header value.
func extractBearer(header string) (string, error) {
	if header == "" {
		return "", errors.New("empty header")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", errors.New("missing Bearer prefix")
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", errors.New("empty token")
	}
	return tok, nil
}

// GetIdentity retrieves the authenticated user address from the Gin context.
func GetIdentity(c *gin.Context) string {
	v, exists := c.Get(IdentityKey)
	if !exists {
		return ""
	}
	addr, _ := v.(string)
	return addr
}

// IsValidAddr checks that the address has the form "@user@domain".
func IsValidAddr(addr string) bool {
	if len(addr) < 3 {
		return false
	}
	if addr[0] != '@' {
		return false
	}
	rest := addr[1:]
	return strings.Contains(rest, "@")
}

// checkFmsgID queries the fmsgid service for a user address.
// Returns (statusCode, acceptingNew, error).
func checkFmsgID(idURL, addr string) (int, bool, error) {
	url := strings.TrimRight(idURL, "/") + "/fmsgid/" + addr
	resp, err := http.Get(url) //nolint:gosec // URL constructed from trusted config + validated addr
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return http.StatusNotFound, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, false, nil
	}

	var result struct {
		AcceptingNew bool `json:"acceptingNew"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return http.StatusOK, true, nil // assume accepting if parse fails
	}
	return http.StatusOK, result.AcceptingNew, nil
}
