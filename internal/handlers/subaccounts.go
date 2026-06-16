package handlers

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

type SubAccountHandler struct {
	store *apiauth.Store
	idURL string
}

func NewSubAccountHandler(store *apiauth.Store, idURL string) *SubAccountHandler {
	return &SubAccountHandler{store: store, idURL: idURL}
}

type subAccountInput struct {
	Agent        string   `json:"agent"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	KeyExpiresAt string   `json:"key_expires_at"`
}

type rotateKeyInput struct {
	AllowedCIDRs []string `json:"allowed_cidrs"`
	KeyExpiresAt string   `json:"key_expires_at"`
}

type subAccountResponse struct {
	Agent        string   `json:"agent"`
	Addr         string   `json:"addr"`
	KeyID        string   `json:"key_id,omitempty"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	KeyExpiresAt string   `json:"key_expires_at"`
	APIKey       string   `json:"api_key,omitempty"`
}

func (h *SubAccountHandler) List(c *gin.Context) {
	owner, ok := requireRS256Owner(c)
	if !ok {
		return
	}

	max, accounts, err := h.store.List(c.Request.Context(), owner)
	if err != nil {
		log.Printf("sub-accounts list: owner=%s: %v", owner, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sub-accounts"})
		return
	}

	out := make([]subAccountResponse, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, subAccountResponse{
			Agent:        a.Agent,
			Addr:         a.Addr,
			KeyID:        a.KeyID,
			AllowedCIDRs: a.AllowedCIDRs,
			KeyExpiresAt: a.KeyExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"max_sub_accounts": max, "sub_accounts": out})
}

func (h *SubAccountHandler) Create(c *gin.Context) {
	owner, ok := requireRS256Owner(c)
	if !ok {
		return
	}

	var in subAccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	expires, err := parseRequiredExpiry(in.KeyExpiresAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key_expires_at must be a future RFC3339 timestamp"})
		return
	}
	if err := apiauth.ValidateCIDRs(in.AllowedCIDRs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_cidrs must contain valid CIDR ranges"})
		return
	}
	subAddr, err := apiauth.DeriveSubAccountAddr(owner, in.Agent)
	if err != nil || !middleware.IsValidAddr(subAddr) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "agent must be 1-64 letters/digits/dots/hyphens and contain no underscores"})
		return
	}
	if !checkAcceptingFmsgID(c, h.idURL, subAddr) {
		return
	}

	key, hash, err := newPlaintextKey()
	if err != nil {
		log.Printf("sub-account create: key generation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate api key"})
		return
	}
	if err := h.store.Create(c.Request.Context(), owner, in.Agent, subAddr, key.ID, hash, in.AllowedCIDRs, expires); err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusCreated, subAccountResponse{
		Agent:        in.Agent,
		Addr:         subAddr,
		KeyID:        key.ID,
		AllowedCIDRs: in.AllowedCIDRs,
		KeyExpiresAt: expires.UTC().Format(time.RFC3339),
		APIKey:       key.Value,
	})
}

func (h *SubAccountHandler) RotateKey(c *gin.Context) {
	owner, ok := requireRS256Owner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	expectedSubAddr, err := apiauth.DeriveSubAccountAddr(owner, agent)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	if !checkAcceptingFmsgID(c, h.idURL, expectedSubAddr) {
		return
	}

	var in rotateKeyInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	expires, err := parseRequiredExpiry(in.KeyExpiresAt)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key_expires_at must be a future RFC3339 timestamp"})
		return
	}
	replaceCIDRs := in.AllowedCIDRs != nil
	if replaceCIDRs {
		if err := apiauth.ValidateCIDRs(in.AllowedCIDRs); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_cidrs must contain valid CIDR ranges"})
			return
		}
	}

	key, hash, err := newPlaintextKey()
	if err != nil {
		log.Printf("sub-account rotate: key generation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate api key"})
		return
	}
	subAddr, err := h.store.RotateKey(c.Request.Context(), owner, agent, key.ID, hash, expires, in.AllowedCIDRs, replaceCIDRs)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"agent":          agent,
		"addr":           subAddr,
		"key_id":         key.ID,
		"key_expires_at": expires.UTC().Format(time.RFC3339),
		"api_key":        key.Value,
	})
}

func (h *SubAccountHandler) Delete(c *gin.Context) {
	owner, ok := requireRS256Owner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	if err := h.store.Delete(c.Request.Context(), owner, agent); err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func requireRS256Owner(c *gin.Context) (string, bool) {
	if middleware.GetAuthType(c) != middleware.AuthTypeRS256 || middleware.GetIdentity(c) != middleware.GetOwnerIdentity(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "RS256 owner authentication is required"})
		return "", false
	}
	return middleware.GetOwnerIdentity(c), true
}

func parseRequiredExpiry(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, errors.New("missing expiry")
	}
	expires, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	if !expires.After(time.Now()) {
		return time.Time{}, errors.New("expiry must be in the future")
	}
	return expires, nil
}

func newPlaintextKey() (apiauth.APIKey, []byte, error) {
	key, err := apiauth.GenerateAPIKey()
	if err != nil {
		return apiauth.APIKey{}, nil, err
	}
	return key, apiauth.HashAPIKey(key.Value), nil
}

func checkAcceptingFmsgID(c *gin.Context, idURL, addr string) bool {
	code, accepting, err := middleware.CheckFmsgID(idURL, addr)
	if err != nil {
		log.Printf("sub-account fmsgid check: addr=%s: %v", addr, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return false
	}
	if code == http.StatusNotFound {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sub-account not found in fmsgid"})
		return false
	}
	if code != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return false
	}
	if !accepting {
		c.JSON(http.StatusForbidden, gin.H{"error": "sub-account is not accepting new messages"})
		return false
	}
	return true
}

func respondSubAccountStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, apiauth.ErrAlreadyExists):
		c.JSON(http.StatusConflict, gin.H{"error": "sub-account already exists"})
	case errors.Is(err, apiauth.ErrLimitExceeded):
		c.JSON(http.StatusForbidden, gin.H{"error": "sub-account limit exceeded"})
	case errors.Is(err, apiauth.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "sub-account not found"})
	default:
		log.Printf("sub-account store error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update sub-account"})
	}
}
