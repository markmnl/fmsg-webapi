// Package middleware configures the JWT authentication middleware.
package middleware

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/sync/singleflight"
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
	// ModeRS256 verifies RS256 JWTs whose public keys are served via JWKS.
	ModeRS256
)

// Config configures the JWT middleware.
type Config struct {
	// Mode selects HMAC (dev) or RS256 (prod) verification.
	Mode Mode

	// HMACKey is the symmetric secret bytes (required when Mode == ModeHMAC).
	HMACKey []byte

	// JWKS resolves RSA public keys (typically by token header `kid`).
	// Required when Mode == ModeRS256.
	JWKS jwt.Keyfunc

	// Issuer, when non-empty, is required to match the token `iss` claim.
	// Mandatory in RS256 mode.
	Issuer string

	// Audience, when non-empty, is required to be present in the token
	// `aud` claim. Mandatory in RS256 mode to pin tokens to the configured
	// application or API.
	Audience string

	// AddressClaim is the JWT claim name carrying the user's fmsg address.
	// Mandatory in RS256 mode because external identity providers usually
	// put provider-specific identifiers in `sub`.
	AddressClaim string

	// IDURL is the base URL of the fmsgid identity service.
	IDURL string

	// ClockSkew is the leeway applied to time-based claim validation.
	// Defaults to DefaultClockSkew when zero.
	ClockSkew time.Duration
}

// Verifier verifies fmsg JWT bearer tokens. It is safe for concurrent use and
// is shared by the Gin authentication middleware and the WebSocket handler,
// which authenticates outside the Gin middleware chain (browsers cannot set an
// Authorization header on a WebSocket connection).
type Verifier struct {
	mode         Mode
	parser       *jwt.Parser
	keyFunc      jwt.Keyfunc
	idURL        string
	addressClaim string
}

// NewVerifier constructs a Verifier from the given configuration.
func NewVerifier(cfg Config) (*Verifier, error) {
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
	case ModeRS256:
		if cfg.JWKS == nil {
			return nil, errors.New("middleware: RS256 mode requires a JWKS keyfunc")
		}
		if cfg.Issuer == "" {
			return nil, errors.New("middleware: RS256 mode requires an Issuer")
		}
		if cfg.Audience == "" {
			return nil, errors.New("middleware: RS256 mode requires an Audience")
		}
		if cfg.AddressClaim == "" {
			return nil, errors.New("middleware: RS256 mode requires an AddressClaim")
		}
		validMethods = []string{jwt.SigningMethodRS256.Alg()}
		jwks := cfg.JWKS
		keyFunc = func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
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

	return &Verifier{
		mode:         cfg.Mode,
		parser:       jwt.NewParser(parserOpts...),
		keyFunc:      keyFunc,
		idURL:        cfg.IDURL,
		addressClaim: cfg.AddressClaim,
	}, nil
}

// Authenticate parses & verifies a bearer token string, validates its claims,
// derives the user's fmsg address, and confirms via fmsgid that the user is
// known and accepting messages.
//
// The address is derived per mode: in RS256 mode the address comes from the
// configured address claim because `sub` is usually a provider-specific
// identifier; in HMAC dev mode the `sub` claim is the address.
//
// On success it returns the user address and http.StatusOK. On failure it
// returns the empty address, an HTTP status (400/401/403/503), and a
// client-safe error message.
func (v *Verifier) Authenticate(tokenStr string) (addr string, status int, msg string) {
	claims := jwt.MapClaims{}
	if _, err := v.parser.ParseWithClaims(tokenStr, claims, v.keyFunc); err != nil {
		log.Printf("auth rejected: reason=parse_error err=%v", err)
		return "", http.StatusUnauthorized, "invalid token"
	}

	switch v.mode {
	case ModeRS256:
		// The token is valid and the user authenticated; a missing address
		// claim just means no fmsg account exists yet, so respond 403
		// rather than 401 (which would trigger client token refreshes).
		addr, _ = claims[v.addressClaim].(string)
		if addr == "" {
			sub, _ := claims["sub"].(string)
			log.Printf("auth rejected: reason=no_address_claim claim=%q sub=%q", v.addressClaim, sub)
			return "", http.StatusForbidden, "no fmsg account for this identity"
		}
	default:
		addr, _ = claims["sub"].(string)
	}

	if !IsValidAddr(addr) {
		log.Printf("auth rejected: reason=invalid_addr addr=%q", addr)
		return "", http.StatusUnauthorized, "invalid identity"
	}

	code, accepting, err := checkFmsgID(v.idURL, addr)
	if err != nil {
		log.Printf("fmsgid check error for %s: %v", addr, err)
		return "", http.StatusServiceUnavailable, "identity service unavailable"
	}
	switch {
	case code == http.StatusNotFound:
		log.Printf("auth rejected: addr=%s reason=not_found", addr)
		return "", http.StatusBadRequest, fmt.Sprintf("User %s not found", addr)
	case code == http.StatusOK && !accepting:
		log.Printf("auth rejected: addr=%s reason=not_accepting", addr)
		return "", http.StatusForbidden, fmt.Sprintf("User %s not authorised to send new messages", addr)
	case code != http.StatusOK:
		log.Printf("auth rejected: addr=%s reason=fmsgid_status=%d", addr, code)
		return "", http.StatusServiceUnavailable, "identity service unavailable"
	}

	return addr, http.StatusOK, ""
}

// New constructs the JWT verification middleware.
//
// The returned handler:
//   - extracts a Bearer token from the Authorization header,
//   - parses & verifies the signature according to cfg.Mode,
//   - validates iss/aud/exp/nbf claims,
//   - derives the user address (RS256: the configured address claim;
//     HMAC: the sub claim) and validates its shape,
//   - calls fmsgid to confirm the user is known and accepting messages,
//   - on success stores the address in the Gin context under IdentityKey.
//
// On failure the response is 400/401/403/503 with a JSON `{"error": "..."}` body.
func New(cfg Config) (gin.HandlerFunc, error) {
	verifier, err := NewVerifier(cfg)
	if err != nil {
		return nil, err
	}

	return func(c *gin.Context) {
		tokenStr, err := extractBearer(c.GetHeader("Authorization"))
		if err != nil {
			respondAuth(c, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}

		addr, status, msg := verifier.Authenticate(tokenStr)
		if status != http.StatusOK {
			respondAuth(c, status, msg)
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

// fmsgIDClient is a dedicated HTTP client with a bounded timeout so that a
// slow or hung fmsgid never blocks an API request goroutine indefinitely
// (which would otherwise hold the inbound HTTP connection open and exhaust
// the browser's per-host connection limit).
var fmsgIDClient = &http.Client{Timeout: 5 * time.Second}

// fmsgIDCacheTTL is how long a positive fmsgid lookup is cached. Tokens are
// re-validated every time, but the relatively expensive network round-trip to
// fmsgid is short-circuited for this window. Negative results are not cached.
const fmsgIDCacheTTL = 30 * time.Second

type fmsgIDEntry struct {
	expires      time.Time
	code         int
	acceptingNew bool
}

var fmsgIDCache sync.Map // map[string]fmsgIDEntry, key = addr

// fmsgIDGroup coalesces concurrent lookups for the same address so that a
// burst of cache misses (e.g. several browser requests arriving before the
// first response is cached) results in a single upstream fmsgid call.
var fmsgIDGroup singleflight.Group

type fmsgIDResult struct {
	code         int
	acceptingNew bool
}

// checkFmsgID queries the fmsgid service for a user address.
// Returns (statusCode, acceptingNew, error). Successful 200 responses are
// cached for fmsgIDCacheTTL to avoid hammering fmsgid when a browser fires
// many concurrent requests with the same JWT. Concurrent cache misses for
// the same address are deduplicated via singleflight.
func checkFmsgID(idURL, addr string) (int, bool, error) {
	if v, ok := fmsgIDCache.Load(addr); ok {
		entry := v.(fmsgIDEntry)
		if time.Now().Before(entry.expires) {
			return entry.code, entry.acceptingNew, nil
		}
		fmsgIDCache.Delete(addr)
	}

	v, err, _ := fmsgIDGroup.Do(addr, func() (interface{}, error) {
		// Re-check inside the singleflight in case another goroutine just
		// populated the cache while we were waiting to enter.
		if v, ok := fmsgIDCache.Load(addr); ok {
			entry := v.(fmsgIDEntry)
			if time.Now().Before(entry.expires) {
				return fmsgIDResult{code: entry.code, acceptingNew: entry.acceptingNew}, nil
			}
		}
		return fetchFmsgID(idURL, addr)
	})
	if err != nil {
		return 0, false, err
	}
	res := v.(fmsgIDResult)
	return res.code, res.acceptingNew, nil
}

// fetchFmsgID performs the actual HTTP call to fmsgid and stores positive
// results in the cache.
func fetchFmsgID(idURL, addr string) (fmsgIDResult, error) {
	url := strings.TrimRight(idURL, "/") + "/fmsgid/" + addr
	resp, err := fmsgIDClient.Get(url) //nolint:gosec // URL constructed from trusted config + validated addr
	if err != nil {
		return fmsgIDResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmsgIDResult{code: http.StatusNotFound}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmsgIDResult{code: resp.StatusCode}, nil
	}

	var result struct {
		AcceptingNew bool `json:"acceptingNew"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return fmsgIDResult{code: http.StatusOK, acceptingNew: true}, nil // assume accepting if parse fails
	}

	fmsgIDCache.Store(addr, fmsgIDEntry{
		expires:      time.Now().Add(fmsgIDCacheTTL),
		code:         http.StatusOK,
		acceptingNew: result.AcceptingNew,
	})
	return fmsgIDResult{code: http.StatusOK, acceptingNew: result.AcceptingNew}, nil
}
