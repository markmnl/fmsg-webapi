// Package middleware configures the JWT authentication middleware.
package middleware

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	jwt "github.com/appleboy/gin-jwt/v2"
	"github.com/gin-gonic/gin"
	jwtv4 "github.com/golang-jwt/jwt/v4"
)

const IdentityKey = "sub"

// clockSkew is the leeway applied to iat/nbf/exp validation to tolerate minor
// clock differences between services (e.g. containers on the same host).
const clockSkew = 10 * time.Second

// identityClaims is the payload stored in the JWT.
type identityClaims struct {
	Addr string
}

// SetupJWT creates and returns a configured GinJWTMiddleware.
// key is the HMAC secret bytes used to validate tokens.
// idURL is the base URL of the fmsgid service used to validate user addresses.
func SetupJWT(key []byte, idURL string) (*jwt.GinJWTMiddleware, error) {
	// Set the global TimeFunc used by golang-jwt/v4 when validating iat/nbf/exp
	// inside MapClaims.Valid(). gin-jwt's own TimeFunc field does not affect this
	// path; only the package-level variable does.
	jwtv4.TimeFunc = func() time.Time { return time.Now().Add(clockSkew) }

	mw, err := jwt.New(&jwt.GinJWTMiddleware{
		Realm:       "fmsg",
		Key:         key,
		Timeout:     24 * time.Hour,
		MaxRefresh:  24 * time.Hour,
		IdentityKey: IdentityKey,

		// PayloadFunc stores the user address from the login credentials into JWT claims.
		PayloadFunc: func(data interface{}) jwt.MapClaims {
			if v, ok := data.(*identityClaims); ok {
				return jwt.MapClaims{IdentityKey: v.Addr}
			}
			return jwt.MapClaims{}
		},

		// IdentityHandler extracts the address from JWT claims and puts it in Gin context.
		IdentityHandler: func(c *gin.Context) interface{} {
			claims := jwt.ExtractClaims(c)
			addr, _ := claims[IdentityKey].(string)
			return &identityClaims{Addr: addr}
		},

		// Authorizator validates the extracted identity and checks with fmsgid.
		Authorizator: func(data interface{}, c *gin.Context) bool {
			v, ok := data.(*identityClaims)
			if !ok || v == nil {
				return false
			}
			addr := v.Addr
			if !isValidAddr(addr) {
				log.Printf("auth rejected: ip=%s reason=invalid_addr", c.ClientIP())
				return false
			}
			// Store the validated identity in context for downstream handlers.
			c.Set(IdentityKey, addr)

			// Validate with fmsgid service.
			code, accepting, err := checkFmsgID(idURL, addr)
			if err != nil {
				log.Printf("fmsgid check error for %s: %v", addr, err)
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
				return false
			}
			if code == http.StatusNotFound {
				log.Printf("auth rejected: ip=%s addr=%s reason=not_found", c.ClientIP(), addr)
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("User %s not found", addr)})
				return false
			}
			if code == http.StatusOK && !accepting {
				log.Printf("auth rejected: ip=%s addr=%s reason=not_accepting", c.ClientIP(), addr)
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("User %s not authorised to send new messages", addr)})
				return false
			}
			return true
		},

		// Unauthorized responds with 401 when JWT validation fails.
		Unauthorized: func(c *gin.Context, code int, message string) {
			log.Printf("auth failure: ip=%s code=%d message=%s", c.ClientIP(), code, message)
			c.JSON(code, gin.H{"error": message})
		},

		TokenLookup:   "header: Authorization",
		TokenHeadName: "Bearer",
		// TimeFunc is used by gin-jwt for orig_iat and expiry arithmetic.
		// Must be kept consistent with the jwtv4.TimeFunc set above.
		TimeFunc: func() time.Time { return time.Now().Add(clockSkew) },
	})
	return mw, err
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

// isValidAddr checks that the address has the form "@user@domain".
func isValidAddr(addr string) bool {
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

	// Parse acceptingNew from the JSON response.
	var result struct {
		AcceptingNew bool `json:"acceptingNew"`
	}
	if err := decodeJSON(resp.Body, &result); err != nil {
		return http.StatusOK, true, nil // assume accepting if parse fails
	}
	return http.StatusOK, result.AcceptingNew, nil
}
