package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsgd/pkg/message"
)

type queries interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

func (h *MessageHandler) q() queries {
	if h.query != nil {
		return h.query
	}
	return h.DB.Pool
}
func (h *AttachmentHandler) q() queries {
	if h.query != nil {
		return h.query
	}
	return h.DB.Pool
}

type finalizationTx struct{ pgx.Tx }

func (t finalizationTx) QueryRow(c context.Context, q string, a ...any) message.Row {
	return t.Tx.QueryRow(c, q, a...)
}
func (t finalizationTx) Query(c context.Context, q string, a ...any) (message.Rows, error) {
	return t.Tx.Query(c, q, a...)
}
func (t finalizationTx) Exec(c context.Context, q string, a ...any) error {
	_, e := t.Tx.Exec(c, q, a...)
	return e
}

// Atomic gives every message mutation one transaction and one message lock.
// A request-local handler copy prevents sharing transactions across requests.
func (h *MessageHandler) Atomic(next func(*MessageHandler, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) { atomic(c, h.DB, func(tx pgx.Tx) { copy := *h; copy.query = tx; next(&copy, c) }) }
}
func (h *AttachmentHandler) Atomic(next func(*AttachmentHandler, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) { atomic(c, h.DB, func(tx pgx.Tx) { copy := *h; copy.query = tx; next(&copy, c) }) }
}

type transactionFiles struct {
	rollback []func()
	commit   []func()
}

func onRollback(c *gin.Context, f func()) {
	if v, ok := c.Get("messageTransactionFiles"); ok {
		v.(*transactionFiles).rollback = append(v.(*transactionFiles).rollback, f)
	}
}
func afterCommit(c *gin.Context, f func()) {
	if v, ok := c.Get("messageTransactionFiles"); ok {
		v.(*transactionFiles).commit = append(v.(*transactionFiles).commit, f)
	} else {
		f()
	}
}
// beginOrReuseTx returns q directly when it is already a pgx.Tx (inside Atomic),
// avoiding a redundant SAVEPOINT round-trip. When reusing an outer transaction,
// commit and rollback are no-ops because Atomic commits on <400 responses and
// rolls back on >=400 responses.
func beginOrReuseTx(ctx context.Context, q queries) (pgx.Tx, func() error, func(), error) {
	if existing, ok := q.(pgx.Tx); ok {
		return existing, func() error { return nil }, func() {}, nil
	}
	tx, err := q.Begin(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return tx, func() error { return tx.Commit(ctx) }, func() { _ = tx.Rollback(ctx) }, nil
}

// limitRequestBody bounds c.Request.Body with http.MaxBytesReader when limit > 0.
func limitRequestBody(c *gin.Context, limit int64) {
	if limit > 0 && c.Request != nil && c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	}
}

// limitJSONBody bounds a JSON message request body carrying a data string of up
// to maxDataSize decoded bytes. A 6x multiplier accommodates worst-case JSON
// string escaping (e.g. HTML characters escaped as \u003c or ASCII-escaped
// non-ASCII runes) plus 1 MiB for envelope metadata.
func limitJSONBody(c *gin.Context, maxDataSize int64) {
	if maxDataSize > 0 {
		limitRequestBody(c, maxDataSize*6+(1<<20))
	}
}

func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func createdFile(c *gin.Context, path string) { onRollback(c, func() { _ = os.Remove(path) }) }

func atomic(c *gin.Context, database *db.DB, next func(pgx.Tx)) {
	ctx := c.Request.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed to begin message transaction"})
		return
	}
	defer tx.Rollback(ctx)
	if c.Param("id") != "" {
		// An add-to batch's hash locks the message it is stored with; routes
		// that take one (react) resolve it again themselves.
		id, _, ok := reactSubject(c, tx)
		if !ok {
			return
		}
		var locked int64
		if err = tx.QueryRow(ctx, `SELECT id FROM msg WHERE id=$1 FOR UPDATE`, id).Scan(&locked); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			c.JSON(500, gin.H{"error": "failed to lock message"})
			return
		}
	}
	files := &transactionFiles{}
	c.Set("messageTransactionFiles", files)
	committed := false
	commitAttempted := false
	defer func() {
		if !committed && !commitAttempted {
			for _, f := range files.rollback {
				f()
			}
		}
	}()
	original := c.Writer
	buffer := &mutationResponse{ResponseWriter: original, status: http.StatusOK}
	c.Writer = buffer
	defer func() { c.Writer = original }()
	next(tx)
	c.Writer = original
	if buffer.status < 400 {
		// A lost commit acknowledgement cannot prove rollback. Retain files
		// rather than risk deleting payloads referenced by a committed row.
		commitAttempted = true
		if err = tx.Commit(ctx); err != nil {
			log.Printf("commit message mutation: %v", err)
			c.JSON(500, gin.H{"error": "failed to commit message mutation"})
			return
		}
		committed = true
		for _, f := range files.commit {
			f()
		}
	} else {
		_ = tx.Rollback(ctx)
	}
	original.WriteHeader(buffer.status)
	_, _ = io.Copy(original, &buffer.body)
}

// HTTP success is buffered until commit; errors discard the transaction.
// Only bounded JSON/no-content mutation responses use this writer.
type mutationResponse struct {
	gin.ResponseWriter
	body   bytes.Buffer
	status int
}

func (w *mutationResponse) WriteHeader(status int)            { w.status = status }
func (w *mutationResponse) WriteHeaderNow()                   {}
func (w *mutationResponse) Write(b []byte) (int, error)       { return w.body.Write(b) }
func (w *mutationResponse) WriteString(s string) (int, error) { return w.body.WriteString(s) }
func (w *mutationResponse) Status() int                       { return w.status }
func (w *mutationResponse) Size() int                         { return w.body.Len() }
func (w *mutationResponse) Written() bool                     { return w.body.Len() > 0 }
func (w *mutationResponse) Flush()                            {}
