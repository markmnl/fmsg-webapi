package handlers

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// threadMaxHops bounds the ancestor walk (defensive; a pid cycle cannot be
// created through the API but the cap keeps the query finite regardless).
const threadMaxHops = 100

// threadEntry is one message on the direct pid lineage, root first.
type threadEntry struct {
	ID       int64
	From     string
	TimeSent *float64
	Type     string
	Size     int
	Body     string // empty when unreadable by the caller or non-text
	Readable bool   // caller is a participant of this message
	Text     bool   // body is a text type that was inlined
}

// renderThreadText renders the lineage as plain text, root at the top. Each
// message gets a one-line separator with sender and send time; non-text
// bodies and messages the caller may not read become placeholders, so the
// chain's shape stays visible without leaking content.
func renderThreadText(entries []threadEntry) string {
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n\n")
		}
		ts := "draft"
		if e.TimeSent != nil {
			ts = time.Unix(int64(*e.TimeSent), 0).UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "--- %s %s ---\n", e.From, ts)
		switch {
		case !e.Readable:
			b.WriteString("[message not visible to you]")
		case !e.Text:
			fmt.Fprintf(&b, "[non-text message: %s, %d bytes]", e.Type, e.Size)
		default:
			b.WriteString(strings.TrimRight(e.Body, "\n"))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// isTextType reports whether a MIME type's body can be inlined as text.
func isTextType(mimeType string) bool {
	t := strings.ToLower(strings.TrimSpace(mimeType))
	return strings.HasPrefix(t, "text/") || t == "application/json"
}

// ThreadText handles GET /fmsg/:id/thread — returns the message's direct
// ancestor lineage (pid walk to the root) plus the message itself as plain
// text, root first. The caller must be a participant of the requested
// message; ancestors the caller is not a participant of appear as
// placeholders (a recipient added mid-thread cannot read what came before).
func (h *MessageHandler) ThreadText(c *gin.Context) {
	addrs, err := h.visibleAddrs(c)
	if err != nil {
		log.Printf("thread text: resolve visible addrs: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	msgID, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	rows, err := h.DB.Pool.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, pid, from_addr, time_sent, type, size, filepath, 0 AS depth
			FROM msg WHERE id = $1
			UNION ALL
			SELECT m.id, m.pid, m.from_addr, m.time_sent, m.type, m.size, m.filepath, c.depth + 1
			FROM msg m JOIN chain c ON m.id = c.pid
			WHERE c.depth < $3
		)
		SELECT c.id, c.from_addr, c.time_sent, c.type, c.size, c.filepath,
		       (c.from_addr = ANY($2)
		        OR EXISTS (SELECT 1 FROM msg_to t WHERE t.msg_id = c.id AND t.addr = ANY($2))
		        OR EXISTS (SELECT 1 FROM msg_add_to a WHERE a.msg_id = c.id AND a.addr = ANY($2))) AS readable
		FROM chain c ORDER BY c.depth DESC`,
		msgID, addrs, threadMaxHops,
	)
	if err != nil {
		log.Printf("thread text: walk %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	defer rows.Close()

	var entries []threadEntry
	paths := map[int64]string{}
	for rows.Next() {
		var e threadEntry
		var dataPath string
		if err := rows.Scan(&e.ID, &e.From, &e.TimeSent, &e.Type, &e.Size, &dataPath, &e.Readable); err != nil {
			log.Printf("thread text: scan: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
			return
		}
		paths[e.ID] = dataPath
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("thread text: rows %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	if len(entries) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}

	// Authorization gate is the requested message itself, same as GET
	// /fmsg/:id — a non-participant learns nothing, not even chain shape.
	if !entries[len(entries)-1].Readable {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	for i := range entries {
		e := &entries[i]
		if !e.Readable || !isTextType(e.Type) {
			continue
		}
		dataPath := paths[e.ID]
		if dataPath == "" {
			continue
		}
		cleanPath, ok := safeDataPath(dataPath, h.DataDir)
		if !ok {
			log.Printf("thread text: path traversal attempt: %s", dataPath)
			continue
		}
		raw, rerr := os.ReadFile(cleanPath)
		if rerr != nil {
			log.Printf("thread text: read %d: %v", e.ID, rerr)
			continue
		}
		e.Body = string(raw)
		e.Text = true
	}

	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(renderThreadText(entries)))
}
