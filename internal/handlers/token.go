package handlers

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
)

type TokenHandler struct {
	store  *apiauth.Store
	issuer *apiauth.TokenIssuer
	idURL  string
}

func NewTokenHandler(store *apiauth.Store, issuer *apiauth.TokenIssuer, idURL string) *TokenHandler {
	return &TokenHandler{store: store, issuer: issuer, idURL: idURL}
}

func (h *TokenHandler) Exchange(c *gin.Context) {
	apiKey, err := bearerTokenStrict(c.GetHeader("Authorization"))
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
		return
	}

	ident, err := h.store.ValidateAPIKey(c.Request.Context(), apiKey, c.ClientIP())
	if err != nil {
		respondTokenError(c, err)
		return
	}
	if !checkAcceptingFmsgID(c, h.idURL, ident.SubAddr) {
		return
	}

	now := time.Now()
	token, expires, err := h.issuer.Mint(ident.OwnerAddr, ident.SubAddr, ident.KeyID, now)
	if err != nil {
		log.Printf("token exchange: mint failed for key_id=%s sub=%s: %v", ident.KeyID, ident.SubAddr, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to mint token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int64(h.issuer.TTL().Seconds()),
		"expires_at":   expires.UTC().Format(time.RFC3339),
	})
}

func respondTokenError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, apiauth.ErrCIDRDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": "source IP not allowed"})
	case errors.Is(err, apiauth.ErrKeyExpired):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "api key expired"})
	case errors.Is(err, apiauth.ErrInvalidRemoteIP):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid source IP"})
	case errors.Is(err, apiauth.ErrInvalidAPIKey):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
	default:
		log.Printf("token exchange: validation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate api key"})
	}
}

func bearerTokenStrict(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", errors.New("missing Bearer prefix")
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", errors.New("empty token")
	}
	return token, nil
}
