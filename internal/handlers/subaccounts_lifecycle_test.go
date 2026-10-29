package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/markmnl/fmsg-webapi/internal/apiauth"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// fakeFmsgID is an in-memory fmsgid serving GET /fmsgid/:address and
// POST /fmsgid with accepting_new, quota_pool and claim.
type fakeFmsgID struct {
	mu        sync.Mutex
	accepting map[string]bool
	pool      map[string]string
	posts     []map[string]any
	// postStatus, when set, is returned by POST /fmsgid without changing anything.
	postStatus int
	// legacy answers POST /fmsgid like an fmsgid without accepting_new: it
	// ignores accepting_new and quota_pool and returns no body.
	legacy bool
}

func newFakeFmsgID(t *testing.T, addrs ...string) (*fakeFmsgID, *httptest.Server) {
	t.Helper()
	f := &fakeFmsgID{accepting: map[string]bool{}, pool: map[string]string{}}
	for _, a := range addrs {
		f.accepting[strings.ToLower(a)] = true
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeFmsgID) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/fmsgid/"):
		accepting, ok := f.accepting[strings.ToLower(strings.TrimPrefix(r.URL.Path, "/fmsgid/"))]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": accepting})
	case r.Method == http.MethodPost && r.URL.Path == "/fmsgid":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.posts = append(f.posts, body)
		if f.postStatus != 0 {
			w.WriteHeader(f.postStatus)
			return
		}
		addr := strings.ToLower(body["address"].(string))
		was, existed := f.accepting[addr]
		// a claim of an existing address outside the pool changes nothing
		if claim, _ := body["claim"].(bool); claim && existed && !f.legacy {
			if p, _ := body["quota_pool"].(string); f.pool[addr] != strings.ToLower(p) {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		if !existed {
			f.accepting[addr] = true
		}
		if f.legacy {
			if existed {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			return
		}
		if a, ok := body["accepting_new"].(bool); ok {
			f.accepting[addr] = a
		}
		if p, ok := body["quota_pool"].(string); ok {
			f.pool[addr] = strings.ToLower(p)
		}
		status := http.StatusCreated
		if existed {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]bool{
			"created":    !existed,
			"reinstated": existed && !was && f.accepting[addr],
		})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeFmsgID) isAccepting(addr string) (accepting, exists bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	accepting, exists = f.accepting[strings.ToLower(addr)]
	return accepting, exists
}

func (f *fakeFmsgID) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func newSubAccountTestStore(t *testing.T) *apiauth.Store {
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
	schema := fmt.Sprintf("sub_account_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
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
	// This service's dd.sql extends the fmsgd schema.
	fmsgdDD := os.Getenv("FMSG_TEST_DD")
	if fmsgdDD == "" {
		fmsgdDD = "../../../fmsgd/dd.sql"
	}
	base, err := os.ReadFile(fmsgdDD)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(base)); err != nil {
		t.Fatal(err)
	}
	dd, err := os.ReadFile("../../dd.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(dd)); err != nil {
		t.Fatal(err)
	}
	return apiauth.NewStore(&db.DB{Pool: pool})
}

const lifecycleOwner = "@alice@example.com"

// subAccountRouter serves the sub-account routes as lifecycleOwner, signed in
// with the identity provider.
func subAccountRouter(h *SubAccountHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.AuthTypeKey, middleware.AuthTypeIdP)
		c.Set(middleware.IdentityKey, lifecycleOwner)
		c.Set(middleware.OwnerIdentityKey, lifecycleOwner)
	})
	r.POST("/fmsg/sub-accounts", h.Create)
	r.DELETE("/fmsg/sub-accounts/:agent", h.Delete)
	return r
}

func serveSubAccount(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func createSubAccount(t *testing.T, r *gin.Engine, agent string, wantCode int) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"agent":%q,"allowed_cidrs":["127.0.0.0/8"],"key_expires_at":%q}`,
		agent, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	w := serveSubAccount(r, http.MethodPost, "/fmsg/sub-accounts", body)
	if w.Code != wantCode {
		t.Fatalf("create %s: status %d, want %d: %s", agent, w.Code, wantCode, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// TestSubAccountLifecycle deletes a derived sub-account, which stops its
// address accepting new messages, and creates it again, which reinstates it.
func TestSubAccountLifecycle(t *testing.T) {
	store := newSubAccountTestStore(t)
	id, server := newFakeFmsgID(t, lifecycleOwner)
	r := subAccountRouter(NewSubAccountHandler(store, server.URL))
	const addr = "@alice_bot@example.com"

	out := createSubAccount(t, r, "bot", http.StatusCreated)
	if out["reinstated"] != false || out["addr"] != addr || out["api_key"] == "" {
		t.Fatalf("new sub-account response: %v", out)
	}
	if accepting, _ := id.isAccepting(addr); !accepting {
		t.Fatal("new sub-account address not accepting new messages")
	}
	if got := id.pool[addr]; got != lifecycleOwner {
		t.Fatalf("new sub-account quota_pool = %q, want the owner %s", got, lifecycleOwner)
	}

	if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/bot", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d: %s", w.Code, w.Body.String())
	}
	if accepting, exists := id.isAccepting(addr); accepting || !exists {
		t.Fatalf("deleted sub-account address: accepting=%v exists=%v, want kept and not accepting", accepting, exists)
	}
	if _, err := store.Get(context.Background(), lifecycleOwner, "bot"); err != apiauth.ErrNotFound {
		t.Fatalf("grant after delete: err=%v, want not found", err)
	}

	out = createSubAccount(t, r, "bot", http.StatusCreated)
	if out["reinstated"] != true {
		t.Fatalf("re-created sub-account response: %v, want reinstated", out)
	}
	if accepting, _ := id.isAccepting(addr); !accepting {
		t.Fatal("re-created sub-account address not accepting new messages")
	}

	// creating it while its grant exists conflicts and leaves it accepting
	createSubAccount(t, r, "bot", http.StatusConflict)
	if accepting, _ := id.isAccepting(addr); !accepting {
		t.Fatal("conflicting create disabled the address")
	}
}

// TestSubAccountDeleteFmsgIDFailure keeps the grant when fmsgid can't stop the
// address accepting new messages, so the delete can be retried.
func TestSubAccountDeleteFmsgIDFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		postStatus int
		legacy     bool
	}{
		{name: "unavailable", postStatus: http.StatusServiceUnavailable},
		{name: "fmsgid without accepting_new", legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newSubAccountTestStore(t)
			id, server := newFakeFmsgID(t, lifecycleOwner)
			r := subAccountRouter(NewSubAccountHandler(store, server.URL))
			createSubAccount(t, r, "bot", http.StatusCreated)

			id.mu.Lock()
			id.postStatus, id.legacy = tc.postStatus, tc.legacy
			id.mu.Unlock()
			if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/bot", ""); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("delete: status %d, want 503", w.Code)
			}
			if _, err := store.Get(context.Background(), lifecycleOwner, "bot"); err != nil {
				t.Fatalf("grant deleted although fmsgid failed: %v", err)
			}

			id.mu.Lock()
			id.postStatus, id.legacy = 0, false
			id.mu.Unlock()
			if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/bot", ""); w.Code != http.StatusNoContent {
				t.Fatalf("retried delete: status %d", w.Code)
			}
			if accepting, _ := id.isAccepting("@alice_bot@example.com"); accepting {
				t.Fatal("address accepting new messages after retried delete")
			}
		})
	}
}

// TestSubAccountDeleteDelegatedIdentity leaves a delegated identity's address
// alone: it is an address in its own right, not one derived for the grant.
func TestSubAccountDeleteDelegatedIdentity(t *testing.T) {
	store := newSubAccountTestStore(t)
	id, server := newFakeFmsgID(t, lifecycleOwner, "@sales@example.com")
	r := subAccountRouter(NewSubAccountHandler(store, server.URL))
	key, err := apiauth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDelegated(context.Background(), lifecycleOwner, "sales", "@sales@example.com", "",
		key.ID, apiauth.HashAPIKey(key.Value), []string{"127.0.0.0/8"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/sales", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", w.Code)
	}
	if n := id.postCount(); n != 0 {
		t.Fatalf("delegated delete sent %d fmsgid updates, want none", n)
	}
	if accepting, _ := id.isAccepting("@sales@example.com"); !accepting {
		t.Fatal("delegated identity stopped accepting new messages")
	}
	if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete missing grant: status %d, want 404", w.Code)
	}
}

// TestSubAccountCreateFailureUndoesReinstatement re-disables a reinstated
// address when its grant can't be created.
func TestSubAccountCreateFailureUndoesReinstatement(t *testing.T) {
	store := newSubAccountTestStore(t)
	id, server := newFakeFmsgID(t, lifecycleOwner)
	r := subAccountRouter(NewSubAccountHandler(store, server.URL))

	createSubAccount(t, r, "old", http.StatusCreated)
	if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/old", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", w.Code)
	}
	for i := range apiauth.DefaultMaxSubAccounts {
		createSubAccount(t, r, fmt.Sprintf("bot%d", i), http.StatusCreated)
	}
	createSubAccount(t, r, "old", http.StatusForbidden)
	if accepting, _ := id.isAccepting("@alice_old@example.com"); accepting {
		t.Fatal("address reinstated although its grant wasn't created")
	}
}

// TestSubAccountCreateRefusesSomeoneElsesAddress: a derived address that
// already exists outside the owner's pool (an account someone else signed up
// for, or a closed one) is refused, and neither fmsgid nor the grants change.
func TestSubAccountCreateRefusesSomeoneElsesAddress(t *testing.T) {
	store := newSubAccountTestStore(t)
	id, server := newFakeFmsgID(t, lifecycleOwner, "@alice_bot@example.com", "@alice_closed@example.com")
	id.accepting["@alice_closed@example.com"] = false
	r := subAccountRouter(NewSubAccountHandler(store, server.URL))

	for _, agent := range []string{"bot", "closed"} {
		createSubAccount(t, r, agent, http.StatusConflict)
	}
	if accepting, _ := id.isAccepting("@alice_closed@example.com"); accepting {
		t.Fatal("refused claim reinstated a closed address")
	}
	if len(id.pool) != 0 {
		t.Fatalf("refused claims pooled %v", id.pool)
	}
	if _, grants, err := store.List(context.Background(), lifecycleOwner); err != nil || len(grants) != 0 {
		t.Fatalf("grants after refused creates: %v, err=%v", grants, err)
	}

	// the owner's own deleted sub-account is still reinstated
	createSubAccount(t, r, "mine", http.StatusCreated)
	if w := serveSubAccount(r, http.MethodDelete, "/fmsg/sub-accounts/mine", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d", w.Code)
	}
	if out := createSubAccount(t, r, "mine", http.StatusCreated); out["reinstated"] != true {
		t.Fatalf("re-create own sub-account: %v, want reinstated", out)
	}
}

// TestSubAccountCreateLegacyFmsgID creates a sub-account with an fmsgid that
// answers registration without a body: it is simply not reinstated.
func TestSubAccountCreateLegacyFmsgID(t *testing.T) {
	store := newSubAccountTestStore(t)
	id, server := newFakeFmsgID(t, lifecycleOwner)
	id.legacy = true
	r := subAccountRouter(NewSubAccountHandler(store, server.URL))
	if out := createSubAccount(t, r, "bot", http.StatusCreated); out["reinstated"] != false {
		t.Fatalf("response %v, want reinstated false", out)
	}
}
