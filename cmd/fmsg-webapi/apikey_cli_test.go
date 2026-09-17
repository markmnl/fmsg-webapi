package main

import (
	"net/http"
	"net/http/httptest"
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

func TestRequireAcceptingCLIAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fmsgid/@alice@exists.test":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"acceptingNew":true}`))
		case "/fmsgid/@alice@disabled.test":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"acceptingNew":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	if err := requireAcceptingCLIAddress(server.URL, "@alice@exists.test", "owner"); err != nil {
		t.Fatalf("existing accepting address: %v", err)
	}
	if err := requireAcceptingCLIAddress(server.URL, "@alice@missing.test", "owner"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing address error = %v", err)
	}
	if err := requireAcceptingCLIAddress(server.URL, "@alice@disabled.test", "owner"); err == nil || !strings.Contains(err.Error(), "not accepting") {
		t.Fatalf("disabled address error = %v", err)
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
