package main

import (
	"strings"
	"testing"
	"time"
)

func TestPrepareCLIGrantInputsAllowsArbitraryDelegatedAddressFlow(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	allowed, gotExpires, key, hash, err := prepareCLIGrantInputs("@mark@example.com", "sales", "203.0.113.0/24", expires)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed[0] != "203.0.113.0/24" {
		t.Fatalf("allowed = %#v", allowed)
	}
	if !gotExpires.After(time.Now()) {
		t.Fatalf("expiry should be in the future: %s", gotExpires)
	}
	if key.Value == "" || len(hash) == 0 {
		t.Fatalf("key/hash not generated")
	}
}

func TestPrepareCLIKeyInputsStillDerivesSubAccountAddress(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	subAddr, _, _, _, _, err := prepareCLIKeyInputs("@mark@example.com", "bot", "203.0.113.0/24", expires)
	if err != nil {
		t.Fatal(err)
	}
	if subAddr != "@mark_bot@example.com" {
		t.Fatalf("subAddr = %q", subAddr)
	}
}

func TestPrepareCLIGrantInputsRejectsInvalidAgent(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, _, _, _, err := prepareCLIGrantInputs("@mark@example.com", "sales_team", "203.0.113.0/24", expires)
	if err == nil || !strings.Contains(err.Error(), "invalid agent") {
		t.Fatalf("err = %v", err)
	}
}
