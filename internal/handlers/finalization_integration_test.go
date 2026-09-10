package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
	"github.com/markmnl/fmsgd/pkg/fmsg"
)

type finalizationAPI struct {
	router *gin.Engine
	h      *MessageHandler
	pool   *pgxpool.Pool
}

func newFinalizationAPI(t *testing.T) *finalizationAPI {
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
	schema := fmt.Sprintf("hash_api_%d", time.Now().UnixNano())
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
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
	dd := os.Getenv("FMSG_TEST_DD")
	if dd == "" {
		dd = "../../../fmsgd/dd.sql"
	}
	sql, err := os.ReadFile(dd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	// Re-running the schema is the supported migration path.
	if _, err = pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	id := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"acceptingNew":true}`)
	}))
	t.Cleanup(id.Close)
	h := NewMessageHandler(&db.DB{Pool: pool}, t.TempDir(), 1<<20, 2<<20, 256, nil, id.URL, "example.com")
	a := NewAttachmentHandler(h.DB, h.DataDir, 1<<20, 2<<20)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(middleware.IdentityKey, c.GetHeader("X-Test-Identity")) })
	r.POST("/fmsg", h.Atomic((*MessageHandler).Create))
	r.PUT("/fmsg/:id", h.Atomic((*MessageHandler).Update))
	r.DELETE("/fmsg/:id", h.Atomic((*MessageHandler).Delete))
	r.GET("/fmsg", h.List)
	r.GET("/fmsg/sent", h.Sent)
	r.GET("/fmsg/:id", h.Get)
	r.GET("/fmsg/:id/data", h.DownloadData)
	r.POST("/fmsg/:id/send", h.Atomic((*MessageHandler).Send))
	r.POST("/fmsg/:id/read", h.MarkRead)
	r.POST("/fmsg/:id/add-to", h.Atomic((*MessageHandler).AddRecipients))
	r.POST("/fmsg/:id/react", h.Atomic((*MessageHandler).React))
	r.GET("/fmsg/:id/thread", h.ThreadText)
	r.GET("/fmsg/:id/thread/messages", h.ThreadMessages)
	r.POST("/fmsg/:id/attach", a.Atomic((*AttachmentHandler).Upload))
	r.GET("/fmsg/:id/attach/:filename", a.Download)
	r.DELETE("/fmsg/:id/attach/:filename", a.Atomic((*AttachmentHandler).DeleteAttachment))
	return &finalizationAPI{r, h, pool}
}
func (a *finalizationAPI) request(identity, method, path string, body any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Identity", identity)
	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, req)
	return w
}
func jsonResponse(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (a *finalizationAPI) draft(t *testing.T, identity, body string, pid any) string {
	t.Helper()
	v := jsonResponse(t, a.request(identity, "POST", "/fmsg", map[string]any{"version": 1, "from": identity, "to": []string{"@bob@example.com", "@alice@example.com"}, "type": "text/plain;charset=UTF-8", "data": body, "pid": pid}), 201)
	return fmt.Sprintf("%.0f", v["id"])
}
func (a *finalizationAPI) upload(t *testing.T, id string) {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	f, err := m.CreateFormFile("file", "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("attachment bytes"))
	_ = m.Close()
	r := httptest.NewRequest("POST", "/fmsg/"+id+"/attach", &b)
	r.Header.Set("Content-Type", m.FormDataContentType())
	r.Header.Set("X-Test-Identity", "@alice@example.com")
	w := httptest.NewRecorder()
	a.router.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("upload %d: %s", w.Code, w.Body)
	}
}

func TestFinalizationLocalHashReferences(t *testing.T) {
	a := newFinalizationAPI(t)
	alice, bob := "@alice@example.com", "@bob@example.com"
	id := a.draft(t, alice, strings.Repeat("compressible content ", 150), nil)
	a.upload(t, id)
	draft := jsonResponse(t, a.request(alice, "GET", "/fmsg/"+id, nil), 200)
	if draft["sha256"] != nil {
		t.Fatal("draft has hash")
	}
	sent := jsonResponse(t, a.request(alice, "POST", "/fmsg/"+id+"/send", nil), 200)
	hash := sent["sha256"].(string)
	if len(hash) != 64 {
		t.Fatal(hash)
	}
	var snapshot, stored []byte
	var stamp float64
	if err := a.pool.QueryRow(context.Background(), `SELECT wire_message,sha256,time_sent FROM msg WHERE id=$1`, id).Scan(&snapshot, &stored, &stamp); err != nil {
		t.Fatal(err)
	}
	wire, err := fmsg.UnmarshalPrepared(snapshot, stored)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Flags&fmsg.FlagDeflate == 0 {
		t.Fatal("body was not compressed before hashing")
	}
	if stamp != sent["time"] {
		t.Fatal("timestamp differs from hashed timestamp")
	}
	for _, suffix := range []string{"", "/data", "/attach/note.txt", "/thread", "/thread/messages"} {
		byID := a.request(bob, "GET", "/fmsg/"+id+suffix, nil)
		byHash := a.request(bob, "GET", "/fmsg/"+strings.ToUpper(hash)+suffix, nil)
		if byID.Code != 200 || byHash.Code != 200 || !bytes.Equal(byID.Body.Bytes(), byHash.Body.Bytes()) {
			t.Fatalf("reference mismatch for %s: %d %d %s", suffix, byID.Code, byHash.Code, byHash.Body)
		}
		denied := a.request("@outsider@example.com", "GET", "/fmsg/"+hash+suffix, nil)
		if denied.Code != 403 {
			t.Fatalf("unauthorized %s: %d", suffix, denied.Code)
		}
	}
	for _, route := range []string{"/fmsg", "/fmsg/sent"} {
		w := a.request(alice, "GET", route, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), hash) {
			t.Fatalf("list missing hash: %s", w.Body)
		}
	}
	item, err := a.h.messageItemFor(context.Background(), mustID(t, id), bob)
	if err != nil || item.SHA256 == nil || *item.SHA256 != hash {
		t.Fatalf("WebSocket payload missing hash: %+v %v", item, err)
	}
	if w := a.request(bob, "POST", "/fmsg/"+hash+"/read", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	for _, path := range []string{strings.Repeat("g", 64), "0", "-1", "18446744073709551616"} {
		if w := a.request(alice, "GET", "/fmsg/"+path, nil); w.Code != 400 {
			t.Fatal(path, w.Code)
		}
	}
	if w := a.request(alice, "GET", "/fmsg/"+strings.Repeat("1", 64), nil); w.Code != 404 {
		t.Fatal("numeric-only hash", w.Code)
	}
	for _, parent := range []any{mustID(t, id), hash} {
		reply := a.draft(t, bob, "local reply", parent)
		jsonResponse(t, a.request(bob, "POST", "/fmsg/"+reply+"/send", nil), 200)
		got := jsonResponse(t, a.request(bob, "GET", "/fmsg/"+reply, nil), 200)
		if got["psha256"] != hash {
			t.Fatal(got)
		}
	}
	batch := jsonResponse(t, a.request(alice, "POST", "/fmsg/"+hash+"/add-to", map[string]any{"add_to": []string{"@carol@example.com"}}), 200)
	batchHash := batch["sha256"].(string)
	if len(batchHash) != 64 || batchHash == hash {
		t.Fatal(batch)
	}
	reply := a.draft(t, "@carol@example.com", "reply to batch", batchHash)
	jsonResponse(t, a.request("@carol@example.com", "POST", "/fmsg/"+reply+"/send", nil), 200)
	got := jsonResponse(t, a.request("@carol@example.com", "GET", "/fmsg/"+reply, nil), 200)
	if got["psha256"] != batchHash {
		t.Fatal(got)
	}
	reaction := jsonResponse(t, a.request(bob, "POST", "/fmsg/"+hash+"/react", map[string]any{"emoji": "👍"}), 201)
	if len(reaction["sha256"].(string)) != 64 {
		t.Fatal(reaction)
	}
	idempotent := jsonResponse(t, a.request(bob, "POST", "/fmsg/"+hash+"/react", map[string]any{"emoji": "👍"}), 200)
	if reaction["sha256"] != idempotent["sha256"] {
		t.Fatal("reaction changed")
	}
	if w := a.request(alice, "DELETE", "/fmsg/"+hash+"/attach/note.txt", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := a.request(alice, "PUT", "/fmsg/"+hash, map[string]any{}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := a.request(alice, "POST", "/fmsg/"+hash+"/send", nil); w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func mustID(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(id, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFinalizationRollbackAndConcurrentSend(t *testing.T) {
	a := newFinalizationAPI(t)
	alice := "@alice@example.com"
	id := a.draft(t, alice, "missing payload", nil)
	var path string
	if err := a.pool.QueryRow(context.Background(), `SELECT filepath FROM msg WHERE id=$1`, id).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if w := a.request(alice, "POST", "/fmsg/"+id+"/send", nil); w.Code != 500 {
		t.Fatal(w.Code, w.Body)
	}
	got := jsonResponse(t, a.request(alice, "GET", "/fmsg/"+id, nil), 200)
	if got["time"] != nil || got["sha256"] != nil {
		t.Fatal("failed finalization committed", got)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".fmsg-wire-*"))
	if len(files) > 0 {
		t.Fatal("failed finalization leaked files", files)
	}
	id = a.draft(t, alice, "race", nil)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- a.request(alice, "POST", "/fmsg/"+id+"/send", nil).Code }()
	}
	wg.Wait()
	close(codes)
	ok := 0
	for c := range codes {
		if c == 200 {
			ok++
		} else if c != 409 {
			t.Fatal(c)
		}
	}
	if ok != 1 {
		t.Fatal("successful sends", ok)
	}
	// Race a full draft edit with send. Either order is valid; the committed
	// digest must describe exactly the content that remains downloadable.
	for i := 0; i < 5; i++ {
		id = a.draft(t, alice, "before", nil)
		wg.Add(2)
		go func(id string) { defer wg.Done(); a.request(alice, "POST", "/fmsg/"+id+"/send", nil) }(id)
		go func(id string) {
			defer wg.Done()
			a.request(alice, "PUT", "/fmsg/"+id, map[string]any{"version": 1, "from": alice, "to": []string{"@bob@example.com"}, "type": "text/plain", "data": "after"})
		}(id)
		wg.Wait()
		var snapshot, hash []byte
		if err := a.pool.QueryRow(context.Background(), `SELECT wire_message,sha256 FROM msg WHERE id=$1`, id).Scan(&snapshot, &hash); err != nil {
			t.Fatal(err)
		}
		h, err := fmsg.UnmarshalPrepared(snapshot, hash)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := os.ReadFile(h.Filepath)
		body := a.request(alice, "GET", "/fmsg/"+hex.EncodeToString(hash)+"/data", nil)
		if body.Code != 200 || !bytes.Equal(wire, body.Body.Bytes()) {
			t.Fatal("content changed after finalization")
		}
	}
}
