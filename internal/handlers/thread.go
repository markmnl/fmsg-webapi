package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/internal/models"
)

// threadMaxHops bounds the ancestor walk (defensive; a pid cycle cannot be
// created through the API but the cap keeps the query finite regardless).
const threadMaxHops = 100

const defaultThreadTextBytes int64 = 32 << 20

type threadBody struct {
	Type      string  `json:"type"`
	Size      int     `json:"size"`
	Text      *string `json:"text,omitempty"`
	Download  string  `json:"download,omitempty"`
	CacheKey  string  `json:"cache_key,omitempty"`
	Cacheable bool    `json:"cacheable"`
}

type threadAttachment struct {
	Position  int    `json:"position"`
	Type      string `json:"type"`
	Filename  string `json:"filename"`
	Size      int    `json:"size"`
	Download  string `json:"download"`
	CacheKey  string `json:"cache_key,omitempty"`
	Cacheable bool   `json:"cacheable"`
}

type threadMessage struct {
	ID            int64               `json:"id"`
	Visible       bool                `json:"visible"`
	Version       int                 `json:"version,omitempty"`
	PID           *int64              `json:"pid,omitempty"`
	NoReply       bool                `json:"no_reply,omitempty"`
	Important     bool                `json:"important,omitempty"`
	Deflate       bool                `json:"deflate,omitempty"`
	Terminal      bool                `json:"terminal,omitempty"`
	From          string              `json:"from,omitempty"`
	To            []string            `json:"to,omitempty"`
	AddTo         []models.AddToBatch `json:"add_to,omitempty"`
	Time          *float64            `json:"time,omitempty"`
	Topic         string              `json:"topic,omitempty"`
	Type          string              `json:"type,omitempty"`
	Size          int                 `json:"size,omitempty"`
	MessageSHA256 string              `json:"message_sha256,omitempty"`
	Body          *threadBody         `json:"body,omitempty"`
	Attachments   []threadAttachment  `json:"attachments,omitempty"`
	dataPath      string
}

type threadMessagesResponse struct {
	RootID    int64           `json:"root_id"`
	TriggerID int64           `json:"trigger_id"`
	Complete  bool            `json:"complete"`
	Messages  []threadMessage `json:"messages"`
}

func partCacheKey(messageHash, kind string, position int, filename string) string {
	if messageHash == "" {
		return ""
	}
	if kind == "body" {
		return "sha256:" + messageHash + ":body"
	}
	return "sha256:" + messageHash + ":attachment:" + strconv.Itoa(position) + ":" + url.PathEscape(filename)
}

func threadDownloadPath(id int64, filename string) string {
	if filename == "" {
		return fmt.Sprintf("/fmsg/%d/data", id)
	}
	return fmt.Sprintf("/fmsg/%d/attach/%s", id, url.PathEscape(filename))
}

func populateThreadBodies(messages []threadMessage, dataDir string, maxTextBytes int64) error {
	var textBytes int64
	for i := range messages {
		m := &messages[i]
		if !m.Visible {
			continue
		}
		key := partCacheKey(m.MessageSHA256, "body", 0, "")
		m.Body = &threadBody{Type: m.Type, Size: m.Size, Download: threadDownloadPath(m.ID, ""), CacheKey: key, Cacheable: key != ""}
		if !isTextType(m.Type) {
			continue
		}
		textBytes += int64(m.Size)
		if textBytes > maxTextBytes {
			return fmt.Errorf("thread text exceeds %d bytes", maxTextBytes)
		}
		cleanPath, ok := safeDataPath(m.dataPath, dataDir)
		if !ok {
			return fmt.Errorf("message %d has an invalid data path", m.ID)
		}
		raw, err := os.ReadFile(cleanPath)
		if err != nil {
			return fmt.Errorf("read message %d body: %w", m.ID, err)
		}
		if utf8.Valid(raw) {
			text := string(raw)
			m.Body.Text = &text
			m.Body.Download = ""
		}
	}
	return nil
}

func loadThreadRelations(ctx context.Context, tx pgx.Tx, messages []threadMessage) error {
	var ids []int64
	byID := make(map[int64]*threadMessage)
	for i := range messages {
		if messages[i].Visible {
			ids = append(ids, messages[i].ID)
			byID[messages[i].ID] = &messages[i]
		}
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := tx.Query(ctx, `SELECT msg_id, addr FROM msg_to WHERE msg_id = ANY($1) ORDER BY msg_id, id`, ids)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var addr string
		if err = rows.Scan(&id, &addr); err != nil {
			rows.Close()
			return err
		}
		byID[id].To = append(byID[id].To, addr)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query(ctx, `
		SELECT b.msg_id, b.id, b.add_to_from, b.time_added, a.addr
		FROM msg_add_to_batch b
		LEFT JOIN msg_add_to a ON a.batch_id = b.id
		WHERE b.msg_id = ANY($1)
		ORDER BY b.msg_id, b.id, a.id`, ids)
	if err != nil {
		return err
	}
	batchIndexes := make(map[int64]int)
	for rows.Next() {
		var msgID, batchID int64
		var from string
		var added float64
		var addr *string
		if err = rows.Scan(&msgID, &batchID, &from, &added, &addr); err != nil {
			rows.Close()
			return err
		}
		idx, ok := batchIndexes[batchID]
		if !ok {
			byID[msgID].AddTo = append(byID[msgID].AddTo, models.AddToBatch{BatchID: batchID, AddToFrom: from, Time: added})
			idx = len(byID[msgID].AddTo) - 1
			batchIndexes[batchID] = idx
		}
		if addr != nil {
			byID[msgID].AddTo[idx].To = append(byID[msgID].AddTo[idx].To, *addr)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query(ctx, `
		SELECT msg_id, position, type, filename, filesize
		FROM msg_attachment WHERE msg_id = ANY($1)
		ORDER BY msg_id, position, filename`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var msgID int64
		var a threadAttachment
		if err = rows.Scan(&msgID, &a.Position, &a.Type, &a.Filename, &a.Size); err != nil {
			return err
		}
		m := byID[msgID]
		a.Download = threadDownloadPath(msgID, a.Filename)
		a.CacheKey = partCacheKey(m.MessageSHA256, "attachment", a.Position, a.Filename)
		a.Cacheable = a.CacheKey != ""
		m.Attachments = append(m.Attachments, a)
	}
	return rows.Err()
}

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
	t := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	return strings.HasPrefix(t, "text/") || t == "application/json" || strings.HasSuffix(t, "+json")
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

// ThreadMessages handles GET /fmsg/:id/thread/messages. It returns the direct
// pid lineage as structured JSON, root first, with textual bodies inlined and
// binary bodies plus attachments represented by authenticated download paths.
func (h *MessageHandler) ThreadMessages(c *gin.Context) {
	addrs, err := h.visibleAddrs(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	msgID, ok := parseID(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	tx, err := h.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		log.Printf("thread messages: begin: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		WITH RECURSIVE chain AS (
			SELECT id, version, pid, no_reply, is_important, is_deflate, is_terminal, time_sent,
			       from_addr, topic, type, size, filepath, sha256, 0 AS depth
			FROM msg WHERE id = $1
			UNION ALL
			SELECT m.id, m.version, m.pid, m.no_reply, m.is_important, m.is_deflate,
			       m.is_terminal, m.time_sent, m.from_addr, m.topic, m.type, m.size, m.filepath,
			       m.sha256, c.depth + 1
			FROM msg m JOIN chain c ON m.id = c.pid
			WHERE c.depth + 1 < $3
		)
		SELECT c.id, c.version, c.pid, c.no_reply, c.is_important, c.is_deflate,
		       c.is_terminal, c.time_sent, c.from_addr, c.topic, c.type, c.size, c.filepath,
		       encode(c.sha256, 'hex'),
		       (c.from_addr = ANY($2)
		        OR EXISTS (SELECT 1 FROM msg_to t WHERE t.msg_id = c.id AND t.addr = ANY($2))
		        OR EXISTS (SELECT 1 FROM msg_add_to a WHERE a.msg_id = c.id AND a.addr = ANY($2)))
		FROM chain c ORDER BY c.depth DESC`, msgID, addrs, threadMaxHops)
	if err != nil {
		log.Printf("thread messages: walk %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	var messages []threadMessage
	for rows.Next() {
		var m threadMessage
		var hash *string
		if err = rows.Scan(&m.ID, &m.Version, &m.PID, &m.NoReply, &m.Important,
			&m.Deflate, &m.Terminal, &m.Time, &m.From, &m.Topic, &m.Type, &m.Size, &m.dataPath,
			&hash, &m.Visible); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
			return
		}
		if hash != nil {
			m.MessageSHA256 = *hash
		}
		messages = append(messages, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	if len(messages) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if !messages[len(messages)-1].Visible {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}
	if len(messages) == threadMaxHops && messages[0].PID != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "thread exceeds maximum ancestry", "code": "thread_too_deep"})
		return
	}
	if err = loadThreadRelations(ctx, tx, messages); err != nil {
		log.Printf("thread messages: relations %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	if err = tx.Commit(ctx); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve thread"})
		return
	}
	if err = populateThreadBodies(messages, h.DataDir, defaultThreadTextBytes); err != nil {
		if strings.Contains(err.Error(), "exceeds") {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error(), "code": "thread_too_large"})
			return
		}
		log.Printf("thread messages: bodies %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "thread data is unavailable", "code": "thread_data_unavailable"})
		return
	}
	complete := true
	for i := range messages {
		if !messages[i].Visible {
			complete = false
			messages[i] = threadMessage{ID: messages[i].ID, Visible: false}
		}
	}
	c.JSON(http.StatusOK, threadMessagesResponse{RootID: messages[0].ID, TriggerID: msgID, Complete: complete, Messages: messages})
}
