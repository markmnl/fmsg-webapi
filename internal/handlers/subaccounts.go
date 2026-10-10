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
	KeyExpiresAt string `json:"key_expires_at"`
}

type updateCIDRsInput struct {
	AllowedCIDRs []string `json:"allowed_cidrs"`
}

type subAccountResponse struct {
	Agent        string   `json:"agent"`
	Addr         string   `json:"addr"`
	GrantType    string   `json:"grant_type"`
	DisplayName  string   `json:"display_name,omitempty"`
	KeyID        string   `json:"key_id,omitempty"`
	AllowedCIDRs []string `json:"allowed_cidrs"`
	KeyExpiresAt string   `json:"key_expires_at"`
	APIKey       string   `json:"api_key,omitempty"`
}

type createSubAccountResponse struct {
	subAccountResponse
	// Reinstated is true when the derived address had been used before (its
	// grant was deleted) and is now accepting new messages again, with its
	// earlier messages still there.
	Reinstated bool `json:"reinstated"`
}

func (h *SubAccountHandler) List(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
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
			GrantType:    a.GrantType,
			DisplayName:  a.DisplayName,
			KeyID:        a.KeyID,
			AllowedCIDRs: a.AllowedCIDRs,
			KeyExpiresAt: a.KeyExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"max_sub_accounts": max, "sub_accounts": out})
}

func (h *SubAccountHandler) Get(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	account, err := h.store.Get(c.Request.Context(), owner, agent)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, subAccountResponse{
		Agent:        account.Agent,
		Addr:         account.Addr,
		GrantType:    account.GrantType,
		DisplayName:  account.DisplayName,
		KeyID:        account.KeyID,
		AllowedCIDRs: account.AllowedCIDRs,
		KeyExpiresAt: account.KeyExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *SubAccountHandler) Create(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
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
	// Derived sub-accounts belong to an already-verified owner, so register
	// them in fmsgid automatically, in the owner's quota pool, rather than
	// requiring a manual step. Only this owner can derive subAddr, so if it
	// already exists there it was theirs before and is reinstated.
	reinstated, err := middleware.RegisterLinkedFmsgID(h.idURL, subAddr, owner)
	if err != nil {
		log.Printf("sub-account create: fmsgid registration: addr=%s: %v", subAddr, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return
	}
	// undo a reinstatement if no grant comes of it, so a deleted address
	// doesn't start accepting new messages again without one
	granted := false
	if reinstated {
		defer func() {
			if !granted {
				disableAfterFailedCreate(h.idURL, subAddr)
			}
		}()
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
	granted = true
	c.JSON(http.StatusCreated, createSubAccountResponse{
		subAccountResponse: subAccountResponse{
			Agent:        in.Agent,
			Addr:         subAddr,
			GrantType:    apiauth.GrantTypeDerivedSubAccount,
			KeyID:        key.ID,
			AllowedCIDRs: in.AllowedCIDRs,
			KeyExpiresAt: expires.UTC().Format(time.RFC3339),
			APIKey:       key.Value,
		},
		Reinstated: reinstated,
	})
}

// disableAfterFailedCreate stops a reinstated derived address accepting new
// messages again after its grant couldn't be created. It is best effort: the
// create has already failed, so a failure here is only logged.
func disableAfterFailedCreate(idURL, addr string) {
	if err := middleware.DisableFmsgID(idURL, addr); err != nil {
		log.Printf("sub-account create: re-disabling reinstated address after failure: addr=%s: %v", addr, err)
	}
}

func (h *SubAccountHandler) RotateKey(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	account, err := h.store.Get(c.Request.Context(), owner, agent)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	if !checkAcceptingFmsgID(c, h.idURL, account.Addr) {
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

	key, hash, err := newPlaintextKey()
	if err != nil {
		log.Printf("sub-account rotate: key generation failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate api key"})
		return
	}
	subAddr, err := h.store.RotateKey(c.Request.Context(), owner, agent, key.ID, hash, expires)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"agent":          agent,
		"addr":           subAddr,
		"grant_type":     account.GrantType,
		"key_id":         key.ID,
		"key_expires_at": expires.UTC().Format(time.RFC3339),
		"api_key":        key.Value,
	})
}

func (h *SubAccountHandler) UpdateCIDRs(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}

	var in updateCIDRsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := apiauth.ValidateCIDRs(in.AllowedCIDRs); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "allowed_cidrs must contain valid CIDR ranges"})
		return
	}

	account, err := h.store.UpdateCIDRs(c.Request.Context(), owner, agent, in.AllowedCIDRs)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, subAccountResponse{
		Agent:        account.Agent,
		Addr:         account.Addr,
		GrantType:    account.GrantType,
		DisplayName:  account.DisplayName,
		KeyID:        account.KeyID,
		AllowedCIDRs: account.AllowedCIDRs,
		KeyExpiresAt: account.KeyExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *SubAccountHandler) Delete(c *gin.Context) {
	owner, ok := requireIdPOwner(c)
	if !ok {
		return
	}
	agent := c.Param("agent")
	if err := apiauth.ValidateAgent(agent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid agent"})
		return
	}
	account, err := h.store.Get(c.Request.Context(), owner, agent)
	if err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	// A derived address exists only for its grant, so stop it accepting new
	// messages: fmsg hosts then refuse mail to it. Do it before deleting the
	// grant so that if fmsgid fails the grant is still there and the owner can
	// retry; deleting first could leave the address accepting mail with
	// nothing left to retry from. Its messages are kept and creating the
	// sub-account again reinstates it. A delegated identity is an address in
	// its own right and is left as it is.
	if account.GrantType == apiauth.GrantTypeDerivedSubAccount {
		if err := middleware.DisableFmsgID(h.idURL, account.Addr); err != nil {
			log.Printf("sub-account delete: fmsgid disable: addr=%s: %v", account.Addr, err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
			return
		}
	}
	if err := h.store.Delete(c.Request.Context(), owner, agent); err != nil {
		respondSubAccountStoreError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func requireIdPOwner(c *gin.Context) (string, bool) {
	if middleware.GetAuthType(c) != middleware.AuthTypeIdP || middleware.GetIdentity(c) != middleware.GetOwnerIdentity(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "identity-provider owner authentication is required"})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "granted address not found in fmsgid"})
		return false
	}
	if code != http.StatusOK {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return false
	}
	if !accepting {
		c.JSON(http.StatusForbidden, gin.H{"error": "granted address is not accepting new messages"})
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
		c.JSON(http.StatusNotFound, gin.H{"error": "grant not found"})
	default:
		log.Printf("grant store error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update grant"})
	}
}
