package apiauth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/internal/db"
)

const DefaultMaxSubAccounts = 5

var (
	ErrNotFound        = errors.New("sub-account not found")
	ErrAlreadyExists   = errors.New("sub-account already exists")
	ErrLimitExceeded   = errors.New("sub-account limit exceeded")
	ErrCIDRDenied      = errors.New("source IP not allowed")
	ErrKeyExpired      = errors.New("api key expired")
	ErrKeyRevoked      = errors.New("api key revoked")
	ErrInvalidRemoteIP = errors.New("invalid source IP")
)

type Store struct {
	DB *db.DB
}

type SubAccount struct {
	OwnerAddr      string
	Agent          string
	Addr           string
	KeyID          string
	AllowedCIDRs   []string
	KeyExpiresAt   time.Time
	MaxSubAccounts int
}

type APIKeyIdentity struct {
	OwnerAddr string
	SubAddr   string
	KeyID     string
}

func NewStore(database *db.DB) *Store {
	return &Store{DB: database}
}

func (s *Store) List(ctx context.Context, ownerAddr string) (int, []SubAccount, error) {
	max, err := s.MaxSubAccounts(ctx, ownerAddr)
	if err != nil {
		return 0, nil, err
	}
	rows, err := s.DB.Pool.Query(ctx,
		`SELECT owner_addr, agent, sub_addr, key_id,
		        ARRAY(SELECT cidr_value::text FROM unnest(allowed_cidrs) AS x(cidr_value)),
		        key_expires_at, max_sub_accounts
		   FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1) AND agent <> ''
		  ORDER BY agent`, ownerAddr)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	var out []SubAccount
	for rows.Next() {
		var a SubAccount
		if err := rows.Scan(&a.OwnerAddr, &a.Agent, &a.Addr, &a.KeyID, &a.AllowedCIDRs, &a.KeyExpiresAt, &a.MaxSubAccounts); err != nil {
			return 0, nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return max, out, nil
}

func (s *Store) MaxSubAccounts(ctx context.Context, ownerAddr string) (int, error) {
	var max int
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT max_sub_accounts
		   FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1) AND agent = ''`, ownerAddr).Scan(&max)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultMaxSubAccounts, nil
	}
	if err != nil {
		return 0, err
	}
	return max, nil
}

func (s *Store) Create(ctx context.Context, ownerAddr, agent, subAddr, keyID string, keyHash []byte, allowedCIDRs []string, keyExpiresAt time.Time) error {
	tx, err := s.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	max, err := maxSubAccountsTx(ctx, tx, ownerAddr)
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)
		   FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1) AND agent <> ''`, ownerAddr).Scan(&count); err != nil {
		return err
	}
	if count >= max {
		return ErrLimitExceeded
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO fmsg_api_sub_account
		        (owner_addr, agent, sub_addr, key_id, key_hash, allowed_cidrs, key_expires_at, max_sub_accounts, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6::cidr[], $7, $8, now())`,
		ownerAddr, agent, subAddr, keyID, keyHash, allowedCIDRs, keyExpiresAt, max)
	if isUniqueViolation(err) {
		return ErrAlreadyExists
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) RotateKey(ctx context.Context, ownerAddr, agent, keyID string, keyHash []byte, keyExpiresAt time.Time, allowedCIDRs []string, replaceCIDRs bool) (string, error) {
	var subAddr string
	var err error
	if replaceCIDRs {
		err = s.DB.Pool.QueryRow(ctx,
			`UPDATE fmsg_api_sub_account
			    SET key_id = $3, key_hash = $4, key_expires_at = $5, allowed_cidrs = $6::cidr[], updated_at = now()
			  WHERE lower(owner_addr) = lower($1) AND agent = $2 AND agent <> ''
			  RETURNING sub_addr`,
			ownerAddr, agent, keyID, keyHash, keyExpiresAt, allowedCIDRs).Scan(&subAddr)
	} else {
		err = s.DB.Pool.QueryRow(ctx,
			`UPDATE fmsg_api_sub_account
			    SET key_id = $3, key_hash = $4, key_expires_at = $5, updated_at = now()
			  WHERE lower(owner_addr) = lower($1) AND agent = $2 AND agent <> ''
			  RETURNING sub_addr`,
			ownerAddr, agent, keyID, keyHash, keyExpiresAt).Scan(&subAddr)
	}
	if isUniqueViolation(err) {
		return "", ErrAlreadyExists
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return subAddr, nil
}

func (s *Store) Delete(ctx context.Context, ownerAddr, agent string) error {
	tag, err := s.DB.Pool.Exec(ctx,
		`DELETE FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1) AND agent = $2 AND agent <> ''`,
		ownerAddr, agent)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ValidateAPIKey(ctx context.Context, apiKey, remoteAddr string) (APIKeyIdentity, error) {
	parsed, err := ParseAPIKey(apiKey)
	if err != nil {
		return APIKeyIdentity{}, err
	}

	var ident APIKeyIdentity
	var hash []byte
	var cidrs []string
	var expires time.Time
	err = s.DB.Pool.QueryRow(ctx,
		`SELECT owner_addr, sub_addr, key_id, key_hash,
		        ARRAY(SELECT cidr_value::text FROM unnest(allowed_cidrs) AS x(cidr_value)),
		        key_expires_at
		   FROM fmsg_api_sub_account
		  WHERE key_id = $1 AND agent <> ''`, parsed.ID).
		Scan(&ident.OwnerAddr, &ident.SubAddr, &ident.KeyID, &hash, &cidrs, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKeyIdentity{}, ErrInvalidAPIKey
	}
	if err != nil {
		return APIKeyIdentity{}, err
	}
	if subtle.ConstantTimeCompare(HashAPIKey(apiKey), hash) != 1 {
		return APIKeyIdentity{}, ErrInvalidAPIKey
	}
	if time.Now().After(expires) {
		return APIKeyIdentity{}, ErrKeyExpired
	}
	if err := remoteAllowed(remoteAddr, cidrs); err != nil {
		return APIKeyIdentity{}, err
	}
	return ident, nil
}

func (s *Store) ValidateToken(ctx context.Context, keyID, ownerAddr, subAddr, remoteAddr string) error {
	var cidrs []string
	var expires time.Time
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT ARRAY(SELECT cidr_value::text FROM unnest(allowed_cidrs) AS x(cidr_value)), key_expires_at
		   FROM fmsg_api_sub_account
		  WHERE key_id = $1
		    AND lower(owner_addr) = lower($2)
		    AND lower(sub_addr) = lower($3)
		    AND agent <> ''`,
		keyID, ownerAddr, subAddr).Scan(&cidrs, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrKeyRevoked
	}
	if err != nil {
		return err
	}
	if time.Now().After(expires) {
		return ErrKeyExpired
	}
	return remoteAllowed(remoteAddr, cidrs)
}

func (s *Store) ValidateActAs(ctx context.Context, ownerAddr, subAddr string) error {
	var exists bool
	err := s.DB.Pool.QueryRow(ctx,
		`SELECT true
		   FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1)
		    AND lower(sub_addr) = lower($2)
		    AND agent <> ''`,
		ownerAddr, subAddr).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return nil
}

func maxSubAccountsTx(ctx context.Context, tx pgx.Tx, ownerAddr string) (int, error) {
	var max int
	err := tx.QueryRow(ctx,
		`SELECT max_sub_accounts
		   FROM fmsg_api_sub_account
		  WHERE lower(owner_addr) = lower($1) AND agent = ''`, ownerAddr).Scan(&max)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultMaxSubAccounts, nil
	}
	if err != nil {
		return 0, err
	}
	return max, nil
}

func remoteAllowed(remoteAddr string, cidrs []string) error {
	addr, err := parseRemoteAddr(remoteAddr)
	if err != nil {
		return err
	}
	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid stored CIDR %q: %w", raw, err)
		}
		if prefix.Contains(addr) {
			return nil
		}
	}
	return ErrCIDRDenied
}

func parseRemoteAddr(remoteAddr string) (netip.Addr, error) {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		remoteAddr = host
	}
	addr, err := netip.ParseAddr(remoteAddr)
	if err != nil {
		return netip.Addr{}, ErrInvalidRemoteIP
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return addr, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "SQLSTATE 23505")
}
