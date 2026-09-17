package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/handlers"
)

func newAPIKeyCLIDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("FMSG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FMSG_TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("apikey_cli_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	dd, err := os.ReadFile("../../dd.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(dd)); err != nil {
		t.Fatal(err)
	}

	// The CLI uses PG* configuration; keep all its writes in this test schema.
	cfg := config.ConnConfig
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGHOST", cfg.Host)
	t.Setenv("PGPORT", strconv.Itoa(int(cfg.Port)))
	t.Setenv("PGUSER", cfg.User)
	t.Setenv("PGPASSWORD", cfg.Password)
	t.Setenv("PGDATABASE", cfg.Database)
	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	if cfg.TLSConfig == nil {
		t.Setenv("PGSSLMODE", "disable")
	} else {
		t.Setenv("PGSSLMODE", "require")
	}
	return &db.DB{Pool: pool}
}

func runCapturedAPIKeyCLI(t *testing.T, args []string) (string, error) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "cli-output")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	previous := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = previous }()
	cliErr := runAPIKeyCLI(context.Background(), args)
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), cliErr
}

func TestAPIKeyCLICreation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		delegated          bool
		existing           bool
		ownerStatus        int
		ownerBody          string
		targetBody         string
		registrationStatus int
		missingAfterCreate bool
		wantErr            string
	}{
		{name: "register derived address"},
		{name: "existing derived address", existing: true},
		{name: "missing owner", ownerStatus: 404, wantErr: "owner @alice@example.com not found"},
		{name: "disabled owner", ownerBody: `{"acceptingNew":false}`, wantErr: "owner @alice@example.com is not accepting"},
		{name: "unavailable owner service", ownerStatus: 503, wantErr: "unexpected status 503"},
		{name: "malformed owner response", ownerBody: "upstream error", wantErr: "checking owner in fmsgid"},
		{name: "registration failure", registrationStatus: 503, wantErr: "registering derived address"},
		{name: "missing after registration", missingAfterCreate: true, wantErr: "derived address @alice_bot@example.com not found"},
		{name: "disabled derived address", existing: true, targetBody: `{"acceptingNew":false}`, wantErr: "derived address @alice_bot@example.com is not accepting"},
		{name: "malformed derived response", targetBody: "upstream error", wantErr: "checking derived address in fmsgid"},
		{name: "existing delegated address", delegated: true, existing: true},
		{name: "missing delegated address", delegated: true, wantErr: "delegated address @sales@example.com not found"},
		{name: "disabled delegated address", delegated: true, existing: true, targetBody: `{"acceptingNew":false}`, wantErr: "delegated address @sales@example.com is not accepting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newAPIKeyCLIDB(t)
			const owner = "@alice@example.com"
			target := "@alice_bot@example.com"
			command := "create"
			if tc.delegated {
				target = "@sales@example.com"
				command = "create-delegation"
			}
			var mu sync.Mutex
			var requests []string
			registered := tc.existing
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/fmsgid/"+owner:
					if tc.ownerStatus != 0 {
						w.WriteHeader(tc.ownerStatus)
					} else if tc.ownerBody != "" {
						_, _ = w.Write([]byte(tc.ownerBody))
					} else {
						_, _ = w.Write([]byte(`{"acceptingNew":true}`))
					}
				case r.Method == http.MethodPost && r.URL.Path == "/fmsgid":
					var body struct{ Address string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Address != target || tc.delegated {
						t.Errorf("unexpected registration: address=%q, err=%v", body.Address, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if tc.registrationStatus != 0 {
						w.WriteHeader(tc.registrationStatus)
						return
					}
					if registered {
						w.WriteHeader(http.StatusOK)
					} else {
						registered = !tc.missingAfterCreate
						w.WriteHeader(http.StatusCreated)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/fmsgid/"+target:
					if !registered {
						w.WriteHeader(http.StatusNotFound)
					} else if tc.targetBody != "" {
						_, _ = w.Write([]byte(tc.targetBody))
					} else {
						_, _ = w.Write([]byte(`{"acceptingNew":true}`))
					}
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			t.Setenv("FMSG_ID_URL", server.URL)
			args := []string{command, "-owner", owner, "-agent", "bot", "-cidr", "127.0.0.0/8", "-expires", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
			if tc.delegated {
				args = append(args, "-addr", target)
			}
			output, err := runCapturedAPIKeyCLI(t, args)
			mu.Lock()
			creationRequests := append([]string(nil), requests...)
			mu.Unlock()
			var count int
			if queryErr := database.Pool.QueryRow(context.Background(), "SELECT count(*) FROM fmsg_api_sub_account").Scan(&count); queryErr != nil {
				t.Fatal(queryErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("creation error = %v, want %q", err, tc.wantErr)
				}
				if count != 0 || output != "" {
					t.Fatalf("failed creation persisted %d grants or printed a key", count)
				}
			} else {
				if err != nil || count != 1 {
					t.Fatalf("creation: grants=%d, err=%v", count, err)
				}
				var key string
				for line := range strings.SplitSeq(output, "\n") {
					if value, ok := strings.CutPrefix(line, "api_key="); ok {
						key = value
					}
				}
				verifyCLIKeyExchange(t, database, server.URL, key, owner, target)
			}

			want := []string{"GET /fmsgid/" + owner}
			if tc.ownerStatus == 0 && tc.ownerBody == "" {
				if !tc.delegated {
					want = append(want, "POST /fmsgid")
				}
				if tc.registrationStatus == 0 {
					want = append(want, "GET /fmsgid/"+target)
				}
			}
			if strings.Join(creationRequests, "\n") != strings.Join(want, "\n") {
				t.Fatalf("creation requests = %v, want %v", creationRequests, want)
			}
		})
	}
}

func exchangeCLIKey(t *testing.T, database *db.DB, idURL, key string) (*httptest.ResponseRecorder, *apiauth.TokenIssuer) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := apiauth.NewTokenIssuer(private, "", "", time.Minute)
	handler := handlers.NewTokenHandler(apiauth.NewStore(database), issuer, idURL)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/fmsg/token", nil)
	c.Request.RemoteAddr = "127.0.0.1:12345"
	c.Request.Header.Set("Authorization", "Bearer "+key)
	handler.Exchange(c)
	return w, issuer
}

func verifyCLIKeyExchange(t *testing.T, database *db.DB, idURL, key, owner, target string) {
	t.Helper()
	w, issuer := exchangeCLIKey(t, database, idURL, key)
	if w.Code != http.StatusOK {
		t.Fatalf("token exchange status = %d", w.Code)
	}
	var response struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var claims apiauth.TokenClaims
	token, err := jwt.ParseWithClaims(response.AccessToken, &claims, func(*jwt.Token) (any, error) {
		return issuer.PublicKey(), nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer(issuer.Issuer()), jwt.WithAudience(issuer.Audience()))
	if err != nil || !token.Valid || claims.Subject != target || claims.OwnerAddr != owner || claims.APIKeyID == "" {
		t.Fatalf("token does not authenticate the expected owner and granted address: %v", err)
	}
}

func TestAPIKeyExchangeRejectsUnregisteredDerivedAddress(t *testing.T) {
	database := newAPIKeyCLIDB(t)
	key, err := apiauth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	const owner = "@alice@example.com"
	const target = "@alice_bot@example.com"
	store := apiauth.NewStore(database)
	if err := store.Create(context.Background(), owner, "bot", target, key.ID, apiauth.HashAPIKey(key.Value), []string{"127.0.0.0/8"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/fmsgid/"+target {
			t.Errorf("unexpected lookup: %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	w, _ := exchangeCLIKey(t, database, server.URL, key.Value)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "granted address not found in fmsgid") {
		t.Fatalf("unregistered address exchange status = %d", w.Code)
	}
}
