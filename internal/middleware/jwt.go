// Package middleware configures authentication middleware.
package middleware

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
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

	AuthTypeIdP = "idp"
	AuthTypeAPI = "api_token"
	// AuthTypeOAuth marks a delegated OAuth token: one issued to a third-party
	// client on the owner's behalf. It never carries owner privileges.
	AuthTypeOAuth = "oauth"
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
	// EdDSA/JWKS provider token verification. Enabled when JWKS is non-nil.
	// Audience is optional; when empty, tokens are not required to carry an
	// aud claim.
	JWKS         jwt.Keyfunc
	Issuer       string
	Audience     string
	AddressClaim string

	// OAuthAudience enables delegated OAuth tokens. A provider token whose aud
	// contains this value is scope-restricted and is never treated as an owner
	// session. It requires Audience, and must differ from it, so that both
	// kinds of token are identified positively by their audience.
	OAuthAudience string

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

	// Scopes and Expires are set for AuthTypeOAuth only.
	Scopes  map[string]struct{}
	Expires time.Time
}

// Verifier verifies fmsg bearer tokens. It is safe for concurrent use and is
// shared by Gin middleware and the WebSocket handler.
type Verifier struct {
	idpParser    *jwt.Parser
	idpKeyFunc   jwt.Keyfunc
	issuer       string
	audience     string
	oauthAud     string
	addressClaim string
	apiParser    *jwt.Parser
	apiPublicKey ed25519.PublicKey
	apiIssuer    string
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
			return nil, errors.New("middleware: EdDSA mode requires an Issuer")
		}
		if cfg.AddressClaim == "" {
			return nil, errors.New("middleware: EdDSA mode requires an AddressClaim")
		}
		if cfg.OAuthAudience != "" && (cfg.Audience == "" || cfg.Audience == cfg.OAuthAudience) {
			return nil, errors.New("middleware: OAuthAudience requires a different, non-empty Audience")
		}
		v.idpKeyFunc = func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
			}
			return cfg.JWKS(t)
		}
		parserOpts := []jwt.ParserOption{
			jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}),
			jwt.WithLeeway(cfg.ClockSkew),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithIssuer(cfg.Issuer),
		}
		// The audience is checked in authenticateIdP: it decides whether a
		// token is an owner session or a delegated OAuth token.
		v.idpParser = jwt.NewParser(parserOpts...)
		v.issuer = cfg.Issuer
		v.audience = cfg.Audience
		v.oauthAud = cfg.OAuthAudience
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
		v.apiIssuer = cfg.APIIssuer
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

	if v.idpParser == nil && v.apiParser == nil {
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
	// When both IdP JWKS and first-party API token auth are configured, peek
	// at the unverified header/claims to route first-party API tokens directly
	// to local Ed25519 verification instead of scanning the JWKS key set first.
	if v.idpParser != nil && v.apiParser != nil && looksLikeAPIToken(tokenStr, v.apiIssuer) {
		res, err := v.authenticateAPIToken(ctx, tokenStr, remoteAddr, actAs)
		if err == nil {
			return res, http.StatusOK, ""
		}
		if status, msg, ok := authFailureFromError(err); ok {
			return authResult{}, status, msg
		}
		res, err = v.authenticateIdP(ctx, tokenStr, actAs)
		if err == nil {
			return res, http.StatusOK, ""
		}
		if status, msg, ok := authFailureFromError(err); ok {
			return authResult{}, status, msg
		}
		log.Printf("auth rejected: reason=parse_error")
		return authResult{}, http.StatusUnauthorized, "invalid token"
	}

	if v.idpParser != nil {
		res, err := v.authenticateIdP(ctx, tokenStr, actAs)
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

// looksLikeAPIToken inspects the unverified JWT header and payload to detect
// first-party API tokens (which carry no kid header and have iss == apiIssuer).
// The token is still cryptographically verified before being accepted.
func looksLikeAPIToken(tokenStr, apiIssuer string) bool {
	if apiIssuer == "" {
		return false
	}
	hdrB64, rest, ok := strings.Cut(tokenStr, ".")
	if !ok {
		return false
	}
	payloadB64, sigB64, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(sigB64, ".") {
		return false
	}
	hdrRaw, err := base64.RawURLEncoding.DecodeString(hdrB64)
	if err != nil {
		return false
	}
	var hdr struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hdrRaw, &hdr); err != nil || hdr.Kid != "" {
		return false
	}
	payloadRaw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payloadRaw, &claims); err != nil {
		return false
	}
	return claims.Iss == apiIssuer
}

func (v *Verifier) authenticateIdP(ctx context.Context, tokenStr, actAs string) (authResult, error) {
	claims := jwt.MapClaims{}
	if _, err := v.idpParser.ParseWithClaims(tokenStr, claims, v.idpKeyFunc); err != nil {
		return authResult{}, err
	}

	delegated, err := v.classifyAudience(claims)
	if err != nil {
		return authResult{}, err
	}

	owner, _ := claims[v.addressClaim].(string)
	if owner == "" {
		sub, _ := claims["sub"].(string)
		log.Printf("auth rejected: reason=no_address_claim claim=%q sub=%q", v.addressClaim, sub)
		return authResult{}, authError{status: http.StatusForbidden, msg: "no fmsg account for this identity"}
	}
	if status, msg := validateIdentity(ctx, owner, v.idURL); status != http.StatusOK {
		return authResult{}, authError{status: status, msg: msg}
	}
	res := authResult{Addr: owner, OwnerAddr: owner, AuthType: AuthTypeIdP}
	var identities []string
	if delegated {
		// Missing or malformed scopes deny; they never fall back to the
		// privileges of an owner session.
		scopes, err := parseScopes(claims)
		if err != nil {
			log.Printf("auth rejected: reason=oauth_scope addr=%s", owner)
			return authResult{}, authError{status: http.StatusForbidden, msg: "missing or malformed scope"}
		}
		if identities, err = delegatedIdentities(claims); err != nil {
			return authResult{}, authError{status: http.StatusForbidden, msg: "malformed identities claim"}
		}
		res.AuthType = AuthTypeOAuth
		res.Scopes = scopes
		if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
			res.Expires = exp.Time
		}
	}

	if strings.TrimSpace(actAs) == "" {
		return res, nil
	}
	if v.apiKeys == nil {
		return authResult{}, authError{status: http.StatusForbidden, msg: "act-as is not enabled"}
	}
	actAs = strings.TrimSpace(actAs)
	// A delegated token may only select identities recorded in its grant; the
	// owner/sub-account relationship below is still required as well.
	if delegated && !slices.Contains(identities, actAs) {
		return authResult{}, authError{status: http.StatusForbidden, msg: "act-as identity is not authorised for this token"}
	}
	if !IsValidAddr(actAs) {
		return authResult{}, authError{status: http.StatusUnauthorized, msg: "invalid act-as identity"}
	}
	if err := v.apiKeys.ValidateActAs(ctx, owner, actAs); err != nil {
		return authResult{}, err
	}
	if status, msg := validateIdentity(ctx, actAs, v.idURL); status != http.StatusOK {
		return authResult{}, authError{status: status, msg: msg}
	}
	res.Addr = actAs
	return res, nil
}

// classifyAudience enforces the intended audience and reports whether the
// token is a delegated OAuth token. With no OAuthAudience configured every
// provider token is an owner session, as before.
func (v *Verifier) classifyAudience(claims jwt.MapClaims) (delegated bool, err error) {
	if v.audience == "" {
		return false, nil // OAuthAudience cannot be set without Audience
	}
	aud, err := claims.GetAudience()
	if err != nil {
		return false, err
	}
	ownerAud := audienceContains(aud, v.audience)
	if v.oauthAud == "" {
		if !ownerAud {
			return false, jwt.ErrTokenInvalidAudience
		}
		return false, nil
	}
	oauthAud := audienceContains(aud, v.oauthAud)
	switch {
	case oauthAud && ownerAud:
		return false, authError{status: http.StatusUnauthorized, msg: "ambiguous token audience"}
	case oauthAud:
		return true, nil
	case !ownerAud:
		return false, jwt.ErrTokenInvalidAudience
	}
	// An actor claim marks delegation (RFC 8693); it has no place on an owner
	// session, so refuse it rather than grant owner privileges.
	if _, has := claims["act"]; has {
		return false, authError{status: http.StatusUnauthorized, msg: "delegated token presented with owner audience"}
	}
	return false, nil
}

func (v *Verifier) authenticateAPIToken(ctx context.Context, tokenStr, remoteAddr, actAs string) (authResult, error) {
	if strings.TrimSpace(actAs) != "" {
		return authResult{}, authError{status: http.StatusForbidden, msg: "act-as is only available with identity-provider authentication"}
	}
	claims := &apiauth.TokenClaims{}
	_, err := v.apiParser.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
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
	if status, msg := validateIdentity(ctx, subAddr, v.idURL); status != http.StatusOK {
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
	if ae, ok := errors.AsType[authError](err); ok {
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
		return http.StatusForbidden, "grant not authorised", true
	}
	return 0, "", false
}

func validateIdentity(ctx context.Context, addr, idURL string) (int, string) {
	if !IsValidAddr(addr) {
		log.Printf("auth rejected: reason=invalid_addr addr=%q", addr)
		return http.StatusUnauthorized, "invalid identity"
	}
	code, accepting, err := CheckFmsgIDContext(ctx, idURL, addr)
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
		if !CheckScope(c, res) {
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

// fmsgIDClient is a dedicated HTTP client with a bounded timeout and pooled
// per-host idle connections so fmsgid lookups reuse keep-alive sockets and never
// block an API request goroutine indefinitely.
var fmsgIDClient = newFmsgIDClient()

func newFmsgIDClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 32
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	_ = body.Close()
}

const (
	fmsgIDCacheTTL        = 30 * time.Second
	fmsgIDCacheMaxEntries = 4096
)

type fmsgIDEntry struct {
	expires      time.Time
	code         int
	acceptingNew bool
}

type boundedFmsgIDCache struct {
	mu         sync.RWMutex
	entries    map[string]fmsgIDEntry
	maxEntries int
}

func newBoundedFmsgIDCache(maxEntries int) *boundedFmsgIDCache {
	return &boundedFmsgIDCache{
		entries:    make(map[string]fmsgIDEntry),
		maxEntries: maxEntries,
	}
}

func (c *boundedFmsgIDCache) get(key string, now time.Time) (fmsgIDEntry, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return fmsgIDEntry{}, false
	}
	if !now.Before(entry.expires) {
		c.delete(key)
		return fmsgIDEntry{}, false
	}
	return entry, true
}

func (c *boundedFmsgIDCache) set(key string, entry fmsgIDEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]fmsgIDEntry)
	}
	maxEntries := c.maxEntries
	if maxEntries <= 0 {
		maxEntries = fmsgIDCacheMaxEntries
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxEntries {
		for k, e := range c.entries {
			if !now.Before(e.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxEntries {
			// Evict the soonest-to-expire entries so the cache stays strictly bounded.
			target := maxEntries - max(1, maxEntries/8)
			for len(c.entries) > target {
				var oldestKey string
				var oldestExp time.Time
				first := true
				for k, e := range c.entries {
					if first || e.expires.Before(oldestExp) {
						oldestKey = k
						oldestExp = e.expires
						first = false
					}
				}
				if first {
					break
				}
				delete(c.entries, oldestKey)
			}
		}
	}
	c.entries[key] = entry
}

func (c *boundedFmsgIDCache) delete(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

var fmsgIDCache = newBoundedFmsgIDCache(fmsgIDCacheMaxEntries)

var fmsgIDGroup singleflight.Group

type fmsgIDResult struct {
	code         int
	acceptingNew bool
}

// CheckFmsgID queries the fmsgid service for a user address.
func CheckFmsgID(idURL, addr string) (int, bool, error) {
	return CheckFmsgIDContext(context.Background(), idURL, addr)
}

// CheckFmsgIDContext queries the fmsgid service for a user address using ctx.
func CheckFmsgIDContext(ctx context.Context, idURL, addr string) (int, bool, error) {
	cacheKey := fmsgIDCacheKey(idURL, addr)
	now := time.Now()
	if entry, ok := fmsgIDCache.get(cacheKey, now); ok {
		return entry.code, entry.acceptingNew, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}

	lookupURL := fmsgIDAddressURL(idURL, addr)
	v, err, _ := fmsgIDGroup.Do(cacheKey, func() (any, error) {
		if entry, ok := fmsgIDCache.get(cacheKey, time.Now()); ok {
			return fmsgIDResult{code: entry.code, acceptingNew: entry.acceptingNew}, nil
		}
		// Detach cancellation inside singleflight so one caller cancelling
		// cannot fail another caller sharing the same in-flight lookup.
		reqCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return fetchFmsgID(reqCtx, lookupURL, cacheKey)
	})
	if err != nil {
		return 0, false, err
	}
	res := v.(fmsgIDResult)
	return res.code, res.acceptingNew, nil
}

// fmsgIDAddressReq is the body of fmsgid's POST /fmsgid, which registers an
// address idempotently and applies accepting_new and quota_pool, when given,
// whether or not the address already existed.
type fmsgIDAddressReq struct {
	Address      string `json:"address"`
	AcceptingNew *bool  `json:"accepting_new,omitempty"`
	QuotaPool    string `json:"quota_pool,omitempty"`
	// Claim asks fmsgid to register the address only when it is new or
	// already in QuotaPool, and to answer 409 otherwise.
	Claim bool `json:"claim,omitempty"`
}

// fmsgIDAddressResp is fmsgid's answer to POST /fmsgid. fmsgid versions that
// don't support accepting_new and quota_pool answer with no body.
type fmsgIDAddressResp struct {
	Created    bool `json:"created"`
	Reinstated bool `json:"reinstated"`
}

// ErrLinkedAddressTaken is returned by RegisterLinkedFmsgID when the derived
// address already exists in fmsgid outside the owner's quota pool: someone
// else holds it.
var ErrLinkedAddressTaken = errors.New("derived address is already someone else's")

// errFmsgIDNoAcceptingNew is returned when fmsgid answers POST /fmsgid without
// a body, i.e. it is a version that ignores accepting_new.
var errFmsgIDNoAcceptingNew = errors.New("fmsgid does not support accepting_new on POST /fmsgid; upgrade fmsgid")

// RegisterLinkedFmsgID registers a linked address — a derived sub-account
// address such as @alice_bot@example.com — with fmsgid, idempotently, in its
// owner's quota pool and accepting new messages. The name alone doesn't prove
// the address is the owner's: an identity provider may let someone else hold
// @alice_bot@example.com. So it is claimed (fmsgid's claim): a new address is
// created in the pool, one already in the owner's pool is reinstated, with
// its earlier messages, and any other existing address is refused with
// ErrLinkedAddressTaken. reinstated reports whether it had stopped accepting
// new messages (e.g. its grant was deleted).
func RegisterLinkedFmsgID(idURL, addr, owner string) (reinstated bool, err error) {
	_, reinstated, err = RegisterLinkedFmsgIDContext(context.Background(), idURL, addr, owner)
	return reinstated, err
}

// RegisterLinkedFmsgIDContext is like RegisterLinkedFmsgID, using ctx and
// reporting both whether the address was newly created and whether it was
// reinstated from disabled state.
func RegisterLinkedFmsgIDContext(ctx context.Context, idURL, addr, owner string) (created, reinstated bool, err error) {
	accepting := true
	resp, status, err := postFmsgIDAddress(ctx, idURL, fmsgIDAddressReq{Address: addr, AcceptingNew: &accepting, QuotaPool: owner, Claim: true})
	if err != nil {
		return false, false, err
	}
	if resp == nil {
		return status == http.StatusCreated, false, nil
	}
	return resp.Created, resp.Reinstated, nil
}

// DisableFmsgID stops addr accepting new messages in fmsgid, so fmsg hosts
// refuse new messages to it. The address, its messages and its usage are kept,
// so its owner can reinstate it with RegisterLinkedFmsgID.
func DisableFmsgID(idURL, addr string) error {
	return DisableFmsgIDContext(context.Background(), idURL, addr)
}

// DisableFmsgIDContext is like DisableFmsgID, using ctx for the HTTP request.
func DisableFmsgIDContext(ctx context.Context, idURL, addr string) error {
	accepting := false
	resp, _, err := postFmsgIDAddress(ctx, idURL, fmsgIDAddressReq{Address: addr, AcceptingNew: &accepting})
	if err != nil {
		return err
	}
	if resp == nil {
		return errFmsgIDNoAcceptingNew
	}
	return nil
}

// postFmsgIDAddress POSTs req to fmsgid's /fmsgid and drops the cached lookup
// of the address. The response is nil when fmsgid answers without a body.
func postFmsgIDAddress(ctx context.Context, idURL string, req fmsgIDAddressReq) (*fmsgIDAddressResp, int, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, 0, err
	}

	u := strings.TrimRight(idURL, "/") + "/fmsgid"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := doFmsgID(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer drainAndClose(resp.Body)

	// whatever fmsgid did, the cached lookup may now be stale
	fmsgIDCache.delete(fmsgIDCacheKey(idURL, req.Address))

	if resp.StatusCode == http.StatusConflict && req.Claim {
		return nil, resp.StatusCode, ErrLinkedAddressTaken
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, resp.StatusCode, fmt.Errorf("fmsgid registration failed: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading fmsgid registration response: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, resp.StatusCode, nil
	}
	var out fmsgIDAddressResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decoding fmsgid registration response: %w", err)
	}
	return &out, resp.StatusCode, nil
}

func fmsgIDAddressURL(idURL, addr string) string {
	return strings.TrimRight(idURL, "/") + "/fmsgid/" + url.PathEscape(addr)
}

func fmsgIDCacheKey(idURL, addr string) string {
	return fmsgIDAddressURL(idURL, strings.ToLower(addr))
}

func fetchFmsgID(ctx context.Context, lookupURL, cacheKey string) (fmsgIDResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL, nil)
	if err != nil {
		return fmsgIDResult{}, err
	}
	resp, err := doFmsgID(req)
	if err != nil {
		return fmsgIDResult{}, err
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return fmsgIDResult{code: http.StatusNotFound}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmsgIDResult{code: resp.StatusCode}, nil
	}

	var result struct {
		AcceptingNew *bool `json:"acceptingNew"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return fmsgIDResult{}, fmt.Errorf("decoding fmsgid address: %w", err)
	}
	if result.AcceptingNew == nil {
		return fmsgIDResult{}, errors.New("fmsgid address response is missing acceptingNew")
	}

	now := time.Now()
	fmsgIDCache.set(cacheKey, fmsgIDEntry{
		expires:      now.Add(fmsgIDCacheTTL),
		code:         http.StatusOK,
		acceptingNew: *result.AcceptingNew,
	}, now)
	return fmsgIDResult{code: http.StatusOK, acceptingNew: *result.AcceptingNew}, nil
}
