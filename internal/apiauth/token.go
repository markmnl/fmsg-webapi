package apiauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	DefaultTokenIssuer   = "fmsg-webapi"
	DefaultTokenAudience = "fmsg-webapi"
	DefaultTokenTTL      = 12 * time.Hour
)

type TokenIssuer struct {
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	issuer     string
	audience   string
	ttl        time.Duration
}

type TokenClaims struct {
	OwnerAddr string `json:"owner"`
	APIKeyID  string `json:"api_key_id"`
	jwt.RegisteredClaims
}

func NewTokenIssuer(privateKey ed25519.PrivateKey, issuer, audience string, ttl time.Duration) *TokenIssuer {
	if issuer == "" {
		issuer = DefaultTokenIssuer
	}
	if audience == "" {
		audience = DefaultTokenAudience
	}
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	pub := privateKey.Public().(ed25519.PublicKey)
	return &TokenIssuer{privateKey: privateKey, publicKey: pub, issuer: issuer, audience: audience, ttl: ttl}
}

func (i *TokenIssuer) PublicKey() ed25519.PublicKey {
	return i.publicKey
}

func (i *TokenIssuer) Issuer() string {
	return i.issuer
}

func (i *TokenIssuer) Audience() string {
	return i.audience
}

func (i *TokenIssuer) TTL() time.Duration {
	return i.ttl
}

func (i *TokenIssuer) Mint(ownerAddr, subAddr, keyID string, now time.Time) (string, time.Time, error) {
	expires := now.Add(i.ttl)
	claims := TokenClaims{
		OwnerAddr: ownerAddr,
		APIKeyID:  keyID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			Subject:   subAddr,
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expires),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := tok.SignedString(i.privateKey)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expires, nil
}

func ParseEd25519PrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 private key: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
		copy(key, raw)
		return key, nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	default:
		return nil, errors.New("Ed25519 private key must be a base64-encoded 32-byte seed or 64-byte private key")
	}
}
