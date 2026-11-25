package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

func TestCreateUniqueAttachmentFile_CollisionAndConcurrentAtomic(t *testing.T) {
	dir := t.TempDir()

	// Sequential collisions get incremented suffixes.
	f1, p1, err := createUniqueAttachmentFile(dir, "report.pdf")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	_ = f1.Close()
	if filepath.Base(p1) != "report.pdf" {
		t.Fatalf("first path = %q, want report.pdf", p1)
	}

	f2, p2, err := createUniqueAttachmentFile(dir, "report.pdf")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	_ = f2.Close()
	if filepath.Base(p2) != "report_1.pdf" {
		t.Fatalf("second path = %q, want report_1.pdf", p2)
	}

	// Concurrent creations for the same filename must all succeed with distinct files.
	const workers = 16
	var wg sync.WaitGroup
	paths := make([]string, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := range workers {
		go func(idx int) {
			defer wg.Done()
			f, p, err := createUniqueAttachmentFile(dir, "concurrent.txt")
			if err != nil {
				errs[idx] = err
				return
			}
			_, _ = f.WriteString("data")
			_ = f.Close()
			paths[idx] = p
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, workers)
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d failed: %v", i, errs[i])
		}
		if seen[paths[i]] {
			t.Fatalf("duplicate path returned to concurrent callers: %q", paths[i])
		}
		seen[paths[i]] = true
		if _, err := os.Stat(paths[i]); err != nil {
			t.Fatalf("created file missing: %v", err)
		}
	}
}

func TestLimitRequestBodyRejectsOversizedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"data":"` + strings.Repeat("x", 2048) + `"}`
	c.Request = httptest.NewRequest(http.MethodPost, "/fmsg", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	limitRequestBody(c, 128)
	var in struct {
		Data string `json:"data"`
	}
	err := c.ShouldBindJSON(&in)
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	if !isMaxBytesError(err) {
		t.Fatalf("expected MaxBytesError, got %T: %v", err, err)
	}
}

func TestLimitJSONBody_AllowsEscapedHTMLAndRejectsOversized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const maxDataSize = int64(1 << 20) // 1 MiB

	// Go's encoding/json escapes '<', '>', '&' to 6 bytes (\u003c, \u003e, \u0026).
	// A <= 1 MiB HTML/symbol payload expands to ~3x-5.8x on the wire — well past
	// the old 2x+1MiB cap (3 MiB) — and must still bind cleanly.
	cases := []struct {
		name string
		data string
	}{
		{
			name: "html_table_cells_3x",
			data: strings.Repeat("<td></td>", 116000), // 1,044,000 B decoded -> ~3.36 MB JSON
		},
		{
			name: "angle_brackets_near_6x",
			data: strings.Repeat("<>&", 340000), // 1,020,000 B decoded -> ~6.12 MB JSON
		},
	}

	h := &MessageHandler{MaxDataSize: maxDataSize}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if int64(len(tc.data)) > maxDataSize {
				t.Fatalf("test fixture decoded len %d > maxDataSize %d", len(tc.data), maxDataSize)
			}
			wire, err := json.Marshal(map[string]any{
				"from": "@other@example.com",
				"to":   []string{"@bob@example.com"},
				"type": "text/html",
				"data": tc.data,
			})
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if int64(len(wire)) <= maxDataSize*2+(1<<20) {
				t.Fatalf("fixture wire size %d did not exceed old 2x+1MiB cap %d", len(wire), maxDataSize*2+(1<<20))
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/fmsg", bytes.NewReader(wire))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(middleware.IdentityKey, "@alice@example.com")

			h.Create(c)
			// Should pass JSON binding (no 413) and hit the ownership check (403).
			if w.Code == http.StatusRequestEntityTooLarge {
				t.Fatalf("valid escaped body (wire=%d, decoded=%d) was rejected with 413", len(wire), len(tc.data))
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 after passing body bind; body=%s", w.Code, w.Body.String())
			}
		})
	}

	// A wire body exceeding 6*MaxDataSize + 1 MiB must be rejected with 413.
	t.Run("over_6x_plus_1MiB_returns_413", func(t *testing.T) {
		oversized := `{"from":"@alice@example.com","to":["@bob@example.com"],"data":"` +
			strings.Repeat("a", int(maxDataSize*6+(1<<20)+1024)) + `"}`
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/fmsg", strings.NewReader(oversized))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(middleware.IdentityKey, "@alice@example.com")

		h.Create(c)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized status = %d, want 413; body=%s", w.Code, w.Body.String())
		}
	})
}

func TestResolveIDStashesInContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "42"}}

	// First call parses numeric id and stashes it.
	id, ok := resolveID(c, nil)
	if !ok || id != 42 {
		t.Fatalf("resolveID numeric = (%d, %v), want (42, true)", id, ok)
	}

	// Simulate hash ID already stashed by atomic(): second call must return
	// the stashed ID without querying DB (nil queries would panic otherwise).
	hash := strings.Repeat("a", 64)
	c.Params = gin.Params{{Key: "id", Value: hash}}
	c.Set(resolvedMsgIDKey, cachedResolvedID{raw: hash, id: 77})
	id, ok = resolveID(c, nil)
	if !ok || id != 77 {
		t.Fatalf("resolveID cached hash = (%d, %v), want (77, true)", id, ok)
	}
}

func TestAttachmentCaseInsensitiveOwner(t *testing.T) {
	if !sameAddr("@Alice@Example.COM", "@alice@example.com") {
		t.Fatal("expected case-insensitive owner match")
	}
	if sameAddr("@alice@example.com", "@bob@example.com") {
		t.Fatal("expected distinct addresses not to match")
	}
}
