package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

func runAPIKeyCLI(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: api-key create|rotate|create-delegation|rotate-delegation -owner @user@domain -agent name -cidr 203.0.113.0/24 -expires 2026-12-31T00:00:00Z")
	}
	switch args[0] {
	case "create":
		return runAPIKeyCreate(ctx, args[1:])
	case "rotate":
		return runAPIKeyRotate(ctx, args[1:])
	case "create-delegation":
		return runAPIKeyCreateDelegation(ctx, args[1:])
	case "rotate-delegation":
		return runAPIKeyRotateDelegation(ctx, args[1:])
	default:
		return fmt.Errorf("unknown api-key command %q", args[0])
	}
}

func runAPIKeyCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("api-key create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	owner := fs.String("owner", "", "owner fmsg address")
	agent := fs.String("agent", "", "sub-account agent name")
	cidrs := fs.String("cidr", "", "comma-separated allowed CIDR ranges")
	expiresRaw := fs.String("expires", "", "API key expiry as RFC3339 timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	subAddr, allowed, expires, key, hash, err := prepareCLIKeyInputs(*owner, *agent, *cidrs, *expiresRaw)
	if err != nil {
		return err
	}
	if len(allowed) == 0 {
		return fmt.Errorf("cidr is required for create")
	}
	database, err := db.New(ctx, "")
	if err != nil {
		return err
	}
	defer database.Close()

	store := apiauth.NewStore(database)
	if err := store.Create(ctx, *owner, *agent, subAddr, key.ID, hash, allowed, expires); err != nil {
		return err
	}
	printCLIKey(*owner, *agent, subAddr, key)
	return nil
}

func runAPIKeyRotate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("api-key rotate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	owner := fs.String("owner", "", "owner fmsg address")
	agent := fs.String("agent", "", "sub-account agent name")
	cidrs := fs.String("cidr", "", "comma-separated allowed CIDR ranges; omit to keep existing")
	expiresRaw := fs.String("expires", "", "API key expiry as RFC3339 timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	subAddr, allowed, expires, key, hash, err := prepareCLIKeyInputs(*owner, *agent, *cidrs, *expiresRaw)
	if err != nil {
		return err
	}
	database, err := db.New(ctx, "")
	if err != nil {
		return err
	}
	defer database.Close()

	store := apiauth.NewStore(database)
	gotSubAddr, err := store.RotateKey(ctx, *owner, *agent, key.ID, hash, expires)
	if err != nil {
		return err
	}
	if !strings.EqualFold(gotSubAddr, subAddr) {
		return fmt.Errorf("stored sub-account address %s does not match derived address %s", gotSubAddr, subAddr)
	}
	if strings.TrimSpace(*cidrs) != "" {
		if _, err := store.UpdateCIDRs(ctx, *owner, *agent, allowed); err != nil {
			return err
		}
	}
	printCLIKey(*owner, *agent, subAddr, key)
	return nil
}

func runAPIKeyCreateDelegation(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("api-key create-delegation", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	owner := fs.String("owner", "", "owner fmsg address")
	agent := fs.String("agent", "", "delegation label")
	addr := fs.String("addr", "", "delegated fmsg address")
	displayName := fs.String("display-name", "", "optional display name")
	cidrs := fs.String("cidr", "", "comma-separated allowed CIDR ranges")
	expiresRaw := fs.String("expires", "", "API key expiry as RFC3339 timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	allowed, expires, key, hash, err := prepareCLIGrantInputs(*owner, *agent, *cidrs, *expiresRaw)
	if err != nil {
		return err
	}
	if len(allowed) == 0 {
		return fmt.Errorf("cidr is required for create-delegation")
	}
	if !middleware.IsValidAddr(*addr) {
		return fmt.Errorf("addr must be an fmsg address")
	}
	database, err := db.New(ctx, "")
	if err != nil {
		return err
	}
	defer database.Close()

	store := apiauth.NewStore(database)
	if err := store.CreateDelegated(ctx, *owner, *agent, *addr, *displayName, key.ID, hash, allowed, expires); err != nil {
		return err
	}
	printCLIKey(*owner, *agent, *addr, key)
	return nil
}

func runAPIKeyRotateDelegation(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("api-key rotate-delegation", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	owner := fs.String("owner", "", "owner fmsg address")
	agent := fs.String("agent", "", "delegation label")
	cidrs := fs.String("cidr", "", "comma-separated allowed CIDR ranges; omit to keep existing")
	expiresRaw := fs.String("expires", "", "API key expiry as RFC3339 timestamp")
	if err := fs.Parse(args); err != nil {
		return err
	}

	allowed, expires, key, hash, err := prepareCLIGrantInputs(*owner, *agent, *cidrs, *expiresRaw)
	if err != nil {
		return err
	}
	database, err := db.New(ctx, "")
	if err != nil {
		return err
	}
	defer database.Close()

	store := apiauth.NewStore(database)
	subAddr, err := store.RotateKey(ctx, *owner, *agent, key.ID, hash, expires)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*cidrs) != "" {
		if _, err := store.UpdateCIDRs(ctx, *owner, *agent, allowed); err != nil {
			return err
		}
	}
	printCLIKey(*owner, *agent, subAddr, key)
	return nil
}

func prepareCLIKeyInputs(owner, agent, cidrsRaw, expiresRaw string) (string, []string, time.Time, apiauth.APIKey, []byte, error) {
	allowed, expires, key, hash, err := prepareCLIGrantInputs(owner, agent, cidrsRaw, expiresRaw)
	if err != nil {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	subAddr, err := apiauth.DeriveSubAccountAddr(owner, agent)
	if err != nil {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	return subAddr, allowed, expires, key, hash, nil
}

func prepareCLIGrantInputs(owner, agent, cidrsRaw, expiresRaw string) ([]string, time.Time, apiauth.APIKey, []byte, error) {
	if !middleware.IsValidAddr(owner) {
		return nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("owner must be an fmsg address")
	}
	if err := apiauth.ValidateAgent(agent); err != nil {
		return nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || !expires.After(time.Now()) {
		return nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("expires must be a future RFC3339 timestamp")
	}
	var allowed []string
	if strings.TrimSpace(cidrsRaw) != "" {
		for _, cidr := range strings.Split(cidrsRaw, ",") {
			allowed = append(allowed, strings.TrimSpace(cidr))
		}
	}
	if len(allowed) > 0 {
		if err := apiauth.ValidateCIDRs(allowed); err != nil {
			return nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("invalid CIDR: %w", err)
		}
	}
	key, err := apiauth.GenerateAPIKey()
	if err != nil {
		return nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	return allowed, expires, key, apiauth.HashAPIKey(key.Value), nil
}

func printCLIKey(owner, agent, subAddr string, key apiauth.APIKey) {
	fmt.Printf("owner=%s\n", owner)
	fmt.Printf("agent=%s\n", agent)
	fmt.Printf("sub_addr=%s\n", subAddr)
	fmt.Printf("key_id=%s\n", key.ID)
	fmt.Printf("api_key=%s\n", key.Value)
}
