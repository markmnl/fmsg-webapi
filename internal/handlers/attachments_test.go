package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
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
