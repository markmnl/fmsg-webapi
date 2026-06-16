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
		return fmt.Errorf("usage: api-key create|rotate -owner @user@domain -agent name -cidr 203.0.113.0/24 -expires 2026-12-31T00:00:00Z")
	}
	switch args[0] {
	case "create":
		return runAPIKeyCreate(ctx, args[1:])
	case "rotate":
		return runAPIKeyRotate(ctx, args[1:])
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
	replaceCIDRs := strings.TrimSpace(*cidrs) != ""
	gotSubAddr, err := store.RotateKey(ctx, *owner, *agent, key.ID, hash, expires, allowed, replaceCIDRs)
	if err != nil {
		return err
	}
	if !strings.EqualFold(gotSubAddr, subAddr) {
		return fmt.Errorf("stored sub-account address %s does not match derived address %s", gotSubAddr, subAddr)
	}
	printCLIKey(*owner, *agent, subAddr, key)
	return nil
}

func prepareCLIKeyInputs(owner, agent, cidrsRaw, expiresRaw string) (string, []string, time.Time, apiauth.APIKey, []byte, error) {
	if !middleware.IsValidAddr(owner) {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("owner must be an fmsg address")
	}
	subAddr, err := apiauth.DeriveSubAccountAddr(owner, agent)
	if err != nil {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	expires, err := time.Parse(time.RFC3339, expiresRaw)
	if err != nil || !expires.After(time.Now()) {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("expires must be a future RFC3339 timestamp")
	}
	var allowed []string
	if strings.TrimSpace(cidrsRaw) != "" {
		for _, cidr := range strings.Split(cidrsRaw, ",") {
			allowed = append(allowed, strings.TrimSpace(cidr))
		}
	}
	if len(allowed) > 0 {
		if err := apiauth.ValidateCIDRs(allowed); err != nil {
			return "", nil, time.Time{}, apiauth.APIKey{}, nil, fmt.Errorf("invalid CIDR: %w", err)
		}
	}
	key, err := apiauth.GenerateAPIKey()
	if err != nil {
		return "", nil, time.Time{}, apiauth.APIKey{}, nil, err
	}
	return subAddr, allowed, expires, key, apiauth.HashAPIKey(key.Value), nil
}

func printCLIKey(owner, agent, subAddr string, key apiauth.APIKey) {
	fmt.Printf("owner=%s\n", owner)
	fmt.Printf("agent=%s\n", agent)
	fmt.Printf("sub_addr=%s\n", subAddr)
	fmt.Printf("key_id=%s\n", key.ID)
	fmt.Printf("api_key=%s\n", key.Value)
}
