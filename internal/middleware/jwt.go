// Package middleware configures authentication middleware.
package middleware

import (
	"context"
	"crypto/ed25519"
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

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
)

const (
	IdentityKey      = "sub"
	OwnerIdentityKey = "owner"
	AuthTypeKey      = "auth_type"

	AuthTypeRS256 = "rs256"
	AuthTypeAPI   = "api_token"
)

// DefaultClockSkew is the leeway applied to iat/nbf/exp validation to tolerate
// minor clock differences between services.
const DefaultClockSkew = 10 * time.Second

type APIKeyChecker interface {
	ValidateToken(ctx context.Context, keyID, ownerAddr, subAddr, remoteAddr string) error
	ValidateActAs(ctx context.Context, ownerAddr, subAddr string) error
}

// Config configures authentication.
type Config struct {
	// RS256/JWKS provider token verification. Enabled when JWKS is non-nil.
	JWKS         jwt.Keyfunc
	Issuer       string
	Audience     string
	AddressClaim string

	// Ed25519 first-party API-token verification. Enabled when APIPublicKey is non-empty.
	APIPublicKey ed25519.PublicKey
	APIIssuer    string
	APIAudience  string
	APIKeys      APIKeyChecker

	// IDURL is the base URL of the fmsgid identity service.
	IDURL string

	// ClockSkew is the leeway applied to time-based claim validation.
	// Defaults to DefaultClockSkew when zero.
	ClockSkew time.Duration
}

type authResult struct {
	Addr      string
	OwnerAddr string
	AuthType  string
}

// Verifier verifies fmsg bearer tokens. It is safe for concurrent use and is
// shared by Gin middleware and the WebSocket handler.
type Verifier struct {
	rsParser     *jwt.Parser
	rsKeyFunc    jwt.Keyfunc
	issuer       string
	audience     string
	addressClaim string
	apiParser    *jwt.Parser
	apiPublicKey ed25519.PublicKey
	apiKeys      APIKeyChecker
	idURL        string
}

func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = DefaultClockSkew
	}

	v := &Verifier{idURL: cfg.IDURL}

	if cfg.JWKS != nil {
		if cfg.Issuer == "" {
			return nil, errors.New("middleware: RS256 mode requires an Issuer")
		}
		if cfg.Audience == "" {
			return nil, errors.New("middleware: RS256 mode requires an Audience")
		}
		if cfg.AddressClaim == "" {
			return nil, errors.New("middleware: RS256 mode requires an AddressClaim")
		}
		v.rsKeyFunc = func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
			}
			return cfg.JWKS(t)
		}
		v.rsParser = jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
			jwt.WithLeeway(cfg.ClockSkew),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
		)
		v.issuer = cfg.Issuer
		v.audience = cfg.Audience
		v.addressClaim = cfg.AddressClaim
	}

	if len(cfg.APIPublicKey) > 0 {
		if cfg.APIKeys == nil {
			return nil, errors.New("middleware: API token mode requires an API key checker")
		}
		if cfg.APIIssuer == "" {
			cfg.APIIssuer = apiauth.DefaultTokenIssuer
		}
		if cfg.APIAudience == "" {
			cfg.APIAudience = apiauth.DefaultTokenAudience
		}
		v.apiPublicKey = cfg.APIPublicKey
		v.apiKeys = cfg.APIKeys
		v.apiParser = jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
			jwt.WithLeeway(cfg.ClockSkew),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithIssuer(cfg.APIIssuer),
			jwt.WithAudience(cfg.APIAudience),
		)
	}

	if v.rsParser == nil && v.apiParser == nil {
		return nil, errors.New("middleware: at least one auth mode must be configured")
	}
	return v, nil
}

func (v *Verifier) Authenticate(tokenStr string) (addr string, status int, msg string) {
	res, status, msg := v.AuthenticateRequest(context.Background(), tokenStr, "127.0.0.1", "")
	if status != http.StatusOK {
		return "", status, msg
	}
	return res.Addr, http.StatusOK, ""
}

func (v *Verifier) AuthenticateRequest(ctx context.Context, tokenStr, remoteAddr, actAs string) (authResult, int, string) {
	if v.rsParser != nil {
		res, err := v.authenticateRS256(ctx, tokenStr, actAs)
		if err == nil {
			return res, http.StatusOK, ""
		}
		if status, msg, ok := authFailureFromError(err); ok {
			return authResult{}, status, msg
		}
	}
	if v.apiParser != nil {
		res, err := v.authenticateAPIToken(ctx, tokenStr, remoteAddr, actAs)
		if err == nil {
			return res, http.StatusOK, ""
		}
		if status, msg, ok := authFailureFromError(err); ok {
			return authResult{}, status, msg
		}
	}
	log.Printf("auth rejected: reason=parse_error")
	return authResult{}, http.StatusUnauthorized, "invalid token"
}

func (v *Verifier) authenticateRS256(ctx context.Context, tokenStr, actAs string) (authResult, error) {
	claims := jwt.MapClaims{}
	if _, err := v.rsParser.ParseWithClaims(tokenStr, claims, v.rsKeyFunc); err != nil {
		return authResult{}, err
	}

	owner, _ := claims[v.addressClaim].(string)
	if owner == "" {
		sub, _ := claims["sub"].(string)
		log.Printf("auth rejected: reason=no_address_claim claim=%q sub=%q", v.addressClaim, sub)
		return authResult{}, authError{status: http.StatusForbidden, msg: "no fmsg account for this identity"}
	}
	if status, msg := validateIdentity(owner, v.idURL); status != http.StatusOK {
		return authResult{}, authError{status: status, msg: msg}
	}
	res := authResult{Addr: owner, OwnerAddr: owner, AuthType: AuthTypeRS256}

	if strings.TrimSpace(actAs) == "" {
		return res, nil
	}
	if v.apiKeys == nil {
		return authResult{}, authError{status: http.StatusForbidden, msg: "act-as is not enabled"}
	}
	actAs = strings.TrimSpace(actAs)
	if !IsValidAddr(actAs) {
		return authResult{}, authError{status: http.StatusUnauthorized, msg: "invalid act-as identity"}
	}
	if err := v.apiKeys.ValidateActAs(ctx, owner, actAs); err != nil {
		return authResult{}, err
	}
	if status, msg := validateIdentity(actAs, v.idURL); status != http.StatusOK {
		return authResult{}, authError{status: status, msg: msg}
	}
	res.Addr = actAs
	return res, nil
}

func (v *Verifier) authenticateAPIToken(ctx context.Context, tokenStr, remoteAddr, actAs string) (authResult, error) {
	if strings.TrimSpace(actAs) != "" {
		return authResult{}, authError{status: http.StatusForbidden, msg: "act-as is only available with RS256 authentication"}
	}
	claims := &apiauth.TokenClaims{}
	_, err := v.apiParser.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
		return v.apiPublicKey, nil
	})
	if err != nil {
		return authResult{}, err
	}
	subAddr := claims.Subject
	if !IsValidAddr(subAddr) || !IsValidAddr(claims.OwnerAddr) || claims.APIKeyID == "" {
		return authResult{}, authError{status: http.StatusUnauthorized, msg: "invalid token identity"}
	}
	if err := v.apiKeys.ValidateToken(ctx, claims.APIKeyID, claims.OwnerAddr, subAddr, remoteAddr); err != nil {
		return authResult{}, err
	}
	if status, msg := validateIdentity(subAddr, v.idURL); status != http.StatusOK {
		return authResult{}, authError{status: status, msg: msg}
	}
	return authResult{Addr: subAddr, OwnerAddr: claims.OwnerAddr, AuthType: AuthTypeAPI}, nil
}

type authError struct {
	status int
	msg    string
}

func (e authError) Error() string {
	return e.msg
}

func authFailureFromError(err error) (int, string, bool) {
	var ae authError
	if errors.As(err, &ae) {
		return ae.status, ae.msg, true
	}
	switch {
	case errors.Is(err, apiauth.ErrCIDRDenied):
		return http.StatusForbidden, "source IP not allowed", true
	case errors.Is(err, apiauth.ErrKeyExpired):
		return http.StatusUnauthorized, "api key expired", true
	case errors.Is(err, apiauth.ErrKeyRevoked):
		return http.StatusUnauthorized, "api key revoked", true
	case errors.Is(err, apiauth.ErrInvalidRemoteIP):
		return http.StatusUnauthorized, "invalid source IP", true
	case errors.Is(err, apiauth.ErrNotFound):
		return http.StatusForbidden, "sub-account not authorised", true
	}
	return 0, "", false
}

func validateIdentity(addr, idURL string) (int, string) {
	if !IsValidAddr(addr) {
		log.Printf("auth rejected: reason=invalid_addr addr=%q", addr)
		return http.StatusUnauthorized, "invalid identity"
	}
	code, accepting, err := CheckFmsgID(idURL, addr)
	if err != nil {
		log.Printf("fmsgid check error for %s: %v", addr, err)
		return http.StatusServiceUnavailable, "identity service unavailable"
	}
	switch {
	case code == http.StatusNotFound:
		log.Printf("auth rejected: addr=%s reason=not_found", addr)
		return http.StatusBadRequest, fmt.Sprintf("User %s not found", addr)
	case code == http.StatusOK && !accepting:
		log.Printf("auth rejected: addr=%s reason=not_accepting", addr)
		return http.StatusForbidden, fmt.Sprintf("User %s not authorised to send new messages", addr)
	case code != http.StatusOK:
		log.Printf("auth rejected: addr=%s reason=fmsgid_status=%d", addr, code)
		return http.StatusServiceUnavailable, "identity service unavailable"
	}
	return http.StatusOK, ""
}

// New constructs the authentication middleware.
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

		res, status, msg := verifier.AuthenticateRequest(c.Request.Context(), tokenStr, c.ClientIP(), c.GetHeader("X-FMSG-Act-As"))
		if status != http.StatusOK {
			respondAuth(c, status, msg)
			return
		}

		c.Set(IdentityKey, res.Addr)
		c.Set(OwnerIdentityKey, res.OwnerAddr)
		c.Set(AuthTypeKey, res.AuthType)
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

// GetIdentity retrieves the effective authenticated user address from the Gin context.
func GetIdentity(c *gin.Context) string {
	v, exists := c.Get(IdentityKey)
	if !exists {
		return ""
	}
	addr, _ := v.(string)
	return addr
}

func GetOwnerIdentity(c *gin.Context) string {
	v, exists := c.Get(OwnerIdentityKey)
	if !exists {
		return GetIdentity(c)
	}
	addr, _ := v.(string)
	return addr
}

func GetAuthType(c *gin.Context) string {
	v, exists := c.Get(AuthTypeKey)
	if !exists {
		return ""
	}
	authType, _ := v.(string)
	return authType
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
// slow or hung fmsgid never blocks an API request goroutine indefinitely.
var fmsgIDClient = &http.Client{Timeout: 5 * time.Second}

const fmsgIDCacheTTL = 30 * time.Second

type fmsgIDEntry struct {
	expires      time.Time
	code         int
	acceptingNew bool
}

var fmsgIDCache sync.Map // map[string]fmsgIDEntry, key = addr

var fmsgIDGroup singleflight.Group

type fmsgIDResult struct {
	code         int
	acceptingNew bool
}

// CheckFmsgID queries the fmsgid service for a user address.
func CheckFmsgID(idURL, addr string) (int, bool, error) {
	if v, ok := fmsgIDCache.Load(addr); ok {
		entry := v.(fmsgIDEntry)
		if time.Now().Before(entry.expires) {
			return entry.code, entry.acceptingNew, nil
		}
		fmsgIDCache.Delete(addr)
	}

	v, err, _ := fmsgIDGroup.Do(addr, func() (interface{}, error) {
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
		return fmsgIDResult{code: http.StatusOK, acceptingNew: true}, nil
	}

	fmsgIDCache.Store(addr, fmsgIDEntry{
		expires:      time.Now().Add(fmsgIDCacheTTL),
		code:         http.StatusOK,
		acceptingNew: result.AcceptingNew,
	})
	return fmsgIDResult{code: http.StatusOK, acceptingNew: result.AcceptingNew}, nil
}
