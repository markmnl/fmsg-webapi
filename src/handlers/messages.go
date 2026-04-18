// Package handlers implements HTTP handlers for the fmsg web API.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/db"
	"github.com/markmnl/fmsg-webapi/middleware"
	"github.com/markmnl/fmsg-webapi/models"
)

// MessageHandler holds dependencies for message routes.
type MessageHandler struct {
	DB          *db.DB
	DataDir     string
	MaxDataSize int64
	MaxMsgSize  int64
}

// NewMessageHandler creates a MessageHandler.
func NewMessageHandler(database *db.DB, dataDir string, maxDataSize, maxMsgSize int64) *MessageHandler {
	return &MessageHandler{DB: database, DataDir: dataDir, MaxDataSize: maxDataSize, MaxMsgSize: maxMsgSize}
}

// messageListItem is the JSON shape for each message in the list response.
// It mirrors the single-message response (including an id).
type messageListItem struct {
	ID          int64               `json:"id"`
	Version     int                 `json:"version"`
	HasPid      bool                `json:"has_pid"`
	HasAddTo    bool                `json:"has_add_to"`
	Important   bool                `json:"important"`
	NoReply     bool                `json:"no_reply"`
	Deflate     bool                `json:"deflate"`
	PID         *int64              `json:"pid"`
	From        string              `json:"from"`
	To          []string            `json:"to"`
	AddTo       []string            `json:"add_to"`
	Time        *float64            `json:"time"`
	Topic       string              `json:"topic"`
	Type        string              `json:"type"`
	Size        int                 `json:"size"`
	Attachments []models.Attachment `json:"attachments"`
}

// messageInput is used for JSON binding on Create/Update — includes Data for the message body.
type messageInput struct {
	models.Message
	Data string `json:"data"`
}

// Wait handles GET /fmsg/wait — long-polls for new messages for the
// authenticated user using PostgreSQL LISTEN/NOTIFY on new_msg_to.
func (h *MessageHandler) Wait(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	if identity == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	sinceID := int64(0)
	if v := c.Query("since_id"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since_id"})
			return
		}
		sinceID = parsed
	}

	timeoutSeconds := 25
	if v := c.Query("timeout"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 1 || parsed > 60 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid timeout"})
			return
		}
		timeoutSeconds = parsed
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	latestID, err := h.latestMessageIDForRecipient(ctx, identity, sinceID)
	if err != nil {
		log.Printf("wait messages: initial check: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
		return
	}
	if latestID > sinceID {
		c.JSON(http.StatusOK, gin.H{"has_new": true, "latest_id": latestID})
		return
	}

	conn, err := h.DB.Pool.Acquire(ctx)
	if err != nil {
		log.Printf("wait messages: acquire conn: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
		return
	}
	defer conn.Release()

	if _, err = conn.Exec(ctx, "LISTEN new_msg_to"); err != nil {
		log.Printf("wait messages: LISTEN new_msg_to: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
		return
	}

	// Re-check after LISTEN to avoid missing notifications between the initial
	// query and channel subscription.
	latestID, err = h.latestMessageIDForRecipient(ctx, identity, sinceID)
	if err != nil {
		log.Printf("wait messages: post-listen check: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
		return
	}
	if latestID > sinceID {
		c.JSON(http.StatusOK, gin.H{"has_new": true, "latest_id": latestID})
		return
	}

	for {
		if _, err = conn.Conn().WaitForNotification(ctx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				c.Status(http.StatusNoContent)
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			log.Printf("wait messages: notification: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
			return
		}

		latestID, err = h.latestMessageIDForRecipient(ctx, identity, sinceID)
		if err != nil {
			log.Printf("wait messages: check after notification: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to wait for messages"})
			return
		}
		if latestID > sinceID {
			c.JSON(http.StatusOK, gin.H{"has_new": true, "latest_id": latestID})
			return
		}
	}
}

func (h *MessageHandler) latestMessageIDForRecipient(ctx context.Context, identity string, sinceID int64) (int64, error) {
	var latestID int64
	err := h.DB.Pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(m.id), 0)
		 FROM msg m
		 WHERE m.id > $2
		   AND (
			   EXISTS (SELECT 1 FROM msg_to mt WHERE mt.msg_id = m.id AND mt.addr = $1)
			   OR EXISTS (SELECT 1 FROM msg_add_to mat WHERE mat.msg_id = m.id AND mat.addr = $1)
		   )`,
		identity, sinceID,
	).Scan(&latestID)
	if err != nil {
		return 0, err
	}
	return latestID, nil
}

// List handles GET /api/v1/messages — lists messages where the authenticated user is a recipient.
func (h *MessageHandler) List(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	if identity == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Parse limit query parameter (default 20, max 100).
	limit := 20
	if l := c.Query("limit"); l != "" {
		parsed, err := strconv.Atoi(l)
		if err != nil || parsed < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}

	// Parse offset query parameter (default 0).
	offset := 0
	if o := c.Query("offset"); o != "" {
		parsed, err := strconv.Atoi(o)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset"})
			return
		}
		offset = parsed
	}

	ctx := c.Request.Context()

	rows, err := h.DB.Pool.Query(ctx,
		`SELECT m.id, m.version, m.pid, m.no_reply, m.is_important, m.is_deflate, m.time_sent, m.from_addr, m.topic, m.type, m.size
		 FROM msg m
		 WHERE EXISTS (SELECT 1 FROM msg_to mt WHERE mt.msg_id = m.id AND mt.addr = $1)
		    OR EXISTS (SELECT 1 FROM msg_add_to mat WHERE mat.msg_id = m.id AND mat.addr = $1)
		 ORDER BY m.id DESC
		 LIMIT $2 OFFSET $3`,
		identity, limit, offset,
	)
	if err != nil {
		log.Printf("list messages: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
		return
	}
	defer rows.Close()

	var messages []messageListItem
	var msgIDs []int64
	for rows.Next() {
		var m messageListItem
		if err := rows.Scan(&m.ID, &m.Version, &m.PID, &m.NoReply, &m.Important, &m.Deflate, &m.Time, &m.From, &m.Topic, &m.Type, &m.Size); err != nil {
			log.Printf("list messages scan: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list messages"})
			return
		}
		m.HasPid = m.PID != nil
		messages = append(messages, m)
		msgIDs = append(msgIDs, m.ID)
	}

	if len(messages) == 0 {
		c.JSON(http.StatusOK, []messageListItem{})
		return
	}

	// Batch-load recipients.
	toRows, err := h.DB.Pool.Query(ctx,
		"SELECT msg_id, addr FROM msg_to WHERE msg_id = ANY($1)",
		msgIDs,
	)
	if err == nil {
		toMap := make(map[int64][]string)
		for toRows.Next() {
			var id int64
			var addr string
			if scanErr := toRows.Scan(&id, &addr); scanErr == nil {
				toMap[id] = append(toMap[id], addr)
			}
		}
		toRows.Close()
		for i := range messages {
			messages[i].To = toMap[messages[i].ID]
		}
	}

	// Batch-load add_to recipients.
	addToRows, err := h.DB.Pool.Query(ctx,
		"SELECT msg_id, addr FROM msg_add_to WHERE msg_id = ANY($1)",
		msgIDs,
	)
	if err == nil {
		addToMap := make(map[int64][]string)
		for addToRows.Next() {
			var id int64
			var addr string
			if scanErr := addToRows.Scan(&id, &addr); scanErr == nil {
				addToMap[id] = append(addToMap[id], addr)
			}
		}
		addToRows.Close()
		for i := range messages {
			messages[i].AddTo = addToMap[messages[i].ID]
			messages[i].HasAddTo = len(messages[i].AddTo) > 0
		}
	}

	// Batch-load attachments.
	attRows, err := h.DB.Pool.Query(ctx,
		"SELECT msg_id, filename, filesize FROM msg_attachment WHERE msg_id = ANY($1)",
		msgIDs,
	)
	if err == nil {
		attMap := make(map[int64][]models.Attachment)
		for attRows.Next() {
			var id int64
			var a models.Attachment
			if scanErr := attRows.Scan(&id, &a.Filename, &a.Size); scanErr == nil {
				attMap[id] = append(attMap[id], a)
			}
		}
		attRows.Close()
		for i := range messages {
			messages[i].Attachments = attMap[messages[i].ID]
		}
	}

	c.JSON(http.StatusOK, messages)
}

// Create handles POST /api/v1/messages — creates a draft message.
func (h *MessageHandler) Create(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	if identity == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var msg messageInput
	if err := c.ShouldBindJSON(&msg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Enforce ownership: from must match the JWT identity.
	if msg.From != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "from address must match authenticated user"})
		return
	}

	if len(msg.To) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "to list must not be empty"})
		return
	}

	if int64(len(msg.Data)) > h.MaxDataSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "message data exceeds maximum size"})
		return
	}

	ctx := c.Request.Context()

	// Validate PID references an existing message.
	if msg.PID != nil {
		var exists bool
		err := h.DB.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM msg WHERE id = $1)", *msg.PID).Scan(&exists)
		if err != nil {
			log.Printf("create message: validate pid: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate pid"})
			return
		}
		if !exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("PID %d not found", *msg.PID)})
			return
		}
	}

	// Detect zip (deflate) content by checking for the zip magic bytes.
	msg.Deflate = isZip([]byte(msg.Data))

	// Parse extension from MIME type.
	ext := mimeToExt(msg.Type)

	// Insert message row with empty filepath; update after we know the ID.
	dataSize := len(msg.Data)
	var msgID int64
	err := h.DB.Pool.QueryRow(ctx,
		`INSERT INTO msg (version, pid, no_reply, is_important, is_deflate, from_addr, topic, type, size, filepath, time_sent)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, '', NULL)
 RETURNING id`,
		msg.Version, msg.PID, msg.NoReply, msg.Important, msg.Deflate, msg.From, msg.Topic, msg.Type, dataSize,
	).Scan(&msgID)
	if err != nil {
		log.Printf("create message: insert: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create message"})
		return
	}

	// Build filesystem path and save data.
	dataPath, err := h.saveMessageData(msg.From, msgID, ext, msg.Data)
	if err != nil {
		log.Printf("create message: save data: %v", err)
		// Attempt rollback.
		_, _ = h.DB.Pool.Exec(ctx, "DELETE FROM msg WHERE id = $1", msgID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save message data"})
		return
	}

	// Update filepath in the database.
	if _, err = h.DB.Pool.Exec(ctx, "UPDATE msg SET filepath = $1 WHERE id = $2", dataPath, msgID); err != nil {
		log.Printf("create message: update filepath: %v", err)
	}

	// Insert recipients.
	for _, addr := range msg.To {
		if _, err = h.DB.Pool.Exec(ctx,
			"INSERT INTO msg_to (msg_id, addr) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			msgID, addr,
		); err != nil {
			log.Printf("create message: insert recipient %s: %v", addr, err)
		}
	}

	c.JSON(http.StatusCreated, gin.H{"id": msgID})
}

// Get handles GET /api/v1/messages/:id — retrieves a message.
func (h *MessageHandler) Get(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	msg, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("get message %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	// Authorization: owner or recipient (to or add_to).
	if msg.From != identity && !isRecipient(msg.To, identity) && !isRecipient(msg.AddTo, identity) {
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	c.JSON(http.StatusOK, msg)
}

// DownloadData handles GET /api/v1/messages/:id/data — downloads the message body as a file.
func (h *MessageHandler) DownloadData(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()

	// Fetch message metadata for auth check and file path.
	var fromAddr string
	var dataPath string
	err := h.DB.Pool.QueryRow(ctx,
		"SELECT from_addr, filepath FROM msg WHERE id = $1", msgID,
	).Scan(&fromAddr, &dataPath)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("download data: fetch msg %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	// Authorize: must be owner or recipient.
	if fromAddr != identity {
		var recipientCount int
		if err = h.DB.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM (
				SELECT 1 FROM msg_to WHERE msg_id = $1 AND addr = $2
				UNION ALL
				SELECT 1 FROM msg_add_to WHERE msg_id = $1 AND addr = $2
			) r`, msgID, identity,
		).Scan(&recipientCount); err != nil || recipientCount == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
	}

	if dataPath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "message data not available"})
		return
	}

	// Path traversal protection: ensure the path is within DataDir.
	cleanPath := filepath.Clean(dataPath)
	cleanDataDir := filepath.Clean(h.DataDir)
	if !strings.HasPrefix(cleanPath, cleanDataDir+string(filepath.Separator)) {
		log.Printf("download data: path traversal attempt: %s", dataPath)
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	c.FileAttachment(cleanPath, filepath.Base(cleanPath))
}

// Update handles PUT /api/v1/messages/:id — updates a draft message.
func (h *MessageHandler) Update(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	existing, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("update message %d fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	if existing.From != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner may update a message"})
		return
	}
	if existing.Time != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "sent messages are immutable"})
		return
	}

	var msg messageInput
	if err := c.ShouldBindJSON(&msg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if msg.From != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "from address must match authenticated user"})
		return
	}

	if int64(len(msg.Data)) > h.MaxDataSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "message data exceeds maximum size"})
		return
	}

	// Check total message size (data + existing attachments).
	var attachTotal int64
	if err := h.DB.Pool.QueryRow(ctx,
		"SELECT COALESCE(SUM(filesize), 0) FROM msg_attachment WHERE msg_id = $1",
		msgID,
	).Scan(&attachTotal); err != nil {
		log.Printf("update message %d: total size check: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check message size"})
		return
	}
	if int64(len(msg.Data))+attachTotal > h.MaxMsgSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "total message size exceeds limit"})
		return
	}

	msg.Deflate = isZip([]byte(msg.Data))
	ext := mimeToExt(msg.Type)

	dataPath, err := h.saveMessageData(msg.From, msgID, ext, msg.Data)
	if err != nil {
		log.Printf("update message %d save: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save message data"})
		return
	}

	_, err = h.DB.Pool.Exec(ctx,
		`UPDATE msg SET version=$1, pid=$2, no_reply=$3, is_important=$4, is_deflate=$5, topic=$6, type=$7, size=$8, filepath=$9 WHERE id=$10`,
		msg.Version, msg.PID, msg.NoReply, msg.Important, msg.Deflate, msg.Topic, msg.Type, len(msg.Data), dataPath, msgID,
	)
	if err != nil {
		log.Printf("update message %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update message"})
		return
	}

	// Replace recipients.
	if _, err = h.DB.Pool.Exec(ctx, "DELETE FROM msg_to WHERE msg_id = $1", msgID); err != nil {
		log.Printf("update message %d delete recipients: %v", msgID, err)
	}
	for _, addr := range msg.To {
		if _, err = h.DB.Pool.Exec(ctx,
			"INSERT INTO msg_to (msg_id, addr) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			msgID, addr,
		); err != nil {
			log.Printf("update message %d insert recipient %s: %v", msgID, addr, err)
		}
	}

	c.JSON(http.StatusOK, gin.H{"id": msgID})
}

// Delete handles DELETE /api/v1/messages/:id — deletes a draft message.
func (h *MessageHandler) Delete(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	existing, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("delete message %d fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	if existing.From != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner may delete a message"})
		return
	}
	if existing.Time != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "sent messages cannot be deleted"})
		return
	}

	// Remove attachment files from disk.
	rows, err := h.DB.Pool.Query(ctx, "SELECT filepath FROM msg_attachment WHERE msg_id = $1", msgID)
	if err == nil {
		var paths []string
		for rows.Next() {
			var p string
			if scanErr := rows.Scan(&p); scanErr == nil {
				paths = append(paths, p)
			}
		}
		rows.Close()
		for _, p := range paths {
			_ = os.Remove(p)
		}
	}

	if _, err = h.DB.Pool.Exec(ctx, "DELETE FROM msg_attachment WHERE msg_id = $1", msgID); err != nil {
		log.Printf("delete message %d: delete attachments: %v", msgID, err)
	}
	if _, err = h.DB.Pool.Exec(ctx, "DELETE FROM msg_to WHERE msg_id = $1", msgID); err != nil {
		log.Printf("delete message %d: delete recipients: %v", msgID, err)
	}
	if _, err = h.DB.Pool.Exec(ctx, "DELETE FROM msg_add_to WHERE msg_id = $1", msgID); err != nil {
		log.Printf("delete message %d: delete add_to recipients: %v", msgID, err)
	}

	// Get data filepath before deleting.
	var dataPath string
	_ = h.DB.Pool.QueryRow(ctx, "SELECT filepath FROM msg WHERE id = $1", msgID).Scan(&dataPath)

	if _, err = h.DB.Pool.Exec(ctx, "DELETE FROM msg WHERE id = $1", msgID); err != nil {
		log.Printf("delete message %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete message"})
		return
	}

	if dataPath != "" {
		_ = os.Remove(dataPath)
	}

	c.Status(http.StatusNoContent)
}

// Send handles POST /api/v1/messages/:id/send — marks a message as sent.
func (h *MessageHandler) Send(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	existing, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("send message %d fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	if existing.From != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner may send a message"})
		return
	}
	if existing.Time != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "message already sent"})
		return
	}

	now := float64(time.Now().UnixMicro()) / 1e6
	if _, err = h.DB.Pool.Exec(ctx, "UPDATE msg SET time_sent = $1 WHERE id = $2", now, msgID); err != nil {
		log.Printf("send message %d: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send message"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"id": msgID, "time": now})
}

// addToInput is the JSON shape for the add-to request body.
type addToInput struct {
	AddTo []string `json:"add_to"`
}

// AddRecipients handles POST /api/v1/messages/:id/add-to — adds additional recipients to a message.
func (h *MessageHandler) AddRecipients(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	var input addToInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if len(input.AddTo) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "add_to list must not be empty"})
		return
	}

	ctx := c.Request.Context()

	// Load the message to verify it exists.
	var fromAddr string
	err := h.DB.Pool.QueryRow(ctx,
		"SELECT from_addr FROM msg WHERE id = $1", msgID,
	).Scan(&fromAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("add recipients: fetch msg %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	// Verify the requester is an existing participant (from or msg_to).
	if fromAddr != identity {
		var recipientCount int
		if err = h.DB.Pool.QueryRow(ctx,
			"SELECT COUNT(*) FROM msg_to WHERE msg_id = $1 AND addr = $2", msgID, identity,
		).Scan(&recipientCount); err != nil || recipientCount == 0 {
			c.JSON(http.StatusForbidden, gin.H{"error": "only existing participants may add recipients"})
			return
		}
	}

	// New add_to addresses must be distinct among themselves (case-insensitive).
	if err := checkDistinctRecipients(input.AddTo, nil); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Insert the new add_to recipients.
	for _, addr := range input.AddTo {
		if _, err = h.DB.Pool.Exec(ctx,
			"INSERT INTO msg_add_to (msg_id, addr) VALUES ($1, $2) ON CONFLICT DO NOTHING",
			msgID, addr,
		); err != nil {
			log.Printf("add recipients: insert %s: %v", addr, err)
		}
	}

	c.JSON(http.StatusOK, gin.H{"id": msgID, "added": len(input.AddTo)})
}

// fetchMessage loads a message with its recipients and attachments from the DB.
func (h *MessageHandler) fetchMessage(ctx context.Context, msgID int64) (*models.Message, error) {
	row := h.DB.Pool.QueryRow(ctx,
		`SELECT version, pid, no_reply, is_important, is_deflate, time_sent, from_addr, topic, type, size FROM msg WHERE id = $1`,
		msgID,
	)

	msg := &models.Message{}
	var pid *int64
	var timeSent *float64
	if err := row.Scan(&msg.Version, &pid, &msg.NoReply, &msg.Important, &msg.Deflate, &timeSent, &msg.From, &msg.Topic, &msg.Type, &msg.Size); err != nil {
		return nil, err
	}
	msg.PID = pid
	msg.Time = timeSent
	msg.HasPid = pid != nil

	// Load recipients.
	rows, err := h.DB.Pool.Query(ctx, "SELECT addr FROM msg_to WHERE msg_id = $1", msgID)
	if err == nil {
		for rows.Next() {
			var addr string
			if scanErr := rows.Scan(&addr); scanErr == nil {
				msg.To = append(msg.To, addr)
			}
		}
		rows.Close()
	}

	// Load add_to recipients.
	addToRows, err := h.DB.Pool.Query(ctx, "SELECT addr FROM msg_add_to WHERE msg_id = $1", msgID)
	if err == nil {
		for addToRows.Next() {
			var addr string
			if scanErr := addToRows.Scan(&addr); scanErr == nil {
				msg.AddTo = append(msg.AddTo, addr)
			}
		}
		addToRows.Close()
	}
	msg.HasAddTo = len(msg.AddTo) > 0

	// Load attachments.
	attRows, err := h.DB.Pool.Query(ctx, "SELECT filename, filesize FROM msg_attachment WHERE msg_id = $1", msgID)
	if err == nil {
		for attRows.Next() {
			var a models.Attachment
			if scanErr := attRows.Scan(&a.Filename, &a.Size); scanErr == nil {
				msg.Attachments = append(msg.Attachments, a)
			}
		}
		attRows.Close()
	}

	return msg, nil
}

// saveMessageData writes data to the filesystem and returns the absolute path.
func (h *MessageHandler) saveMessageData(fromAddr string, msgID int64, ext, data string) (string, error) {
	dir := msgDataDir(h.DataDir, fromAddr, msgID)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	filename := "data" + ext
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(data), 0640); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return path, nil
}

// msgDataDir returns <DataDir>/<domain>/<user>/out/<msgID>.
func msgDataDir(dataDir, fromAddr string, msgID int64) string {
	user, domain := parseAddr(fromAddr)
	return filepath.Join(dataDir, domain, user, "out", strconv.FormatInt(msgID, 10))
}

// parseAddr extracts user and domain from "@user@domain".
func parseAddr(addr string) (user, domain string) {
	if len(addr) < 3 {
		return addr, ""
	}
	rest := addr[1:] // "user@domain"
	idx := strings.LastIndex(rest, "@")
	if idx < 0 {
		return rest, ""
	}
	return rest[:idx], rest[idx+1:]
}

// mimeToExt converts a MIME type to a file extension.
func mimeToExt(mimeType string) string {
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return ".bin"
	}
	exts, err := mime.ExtensionsByType(mediaType)
	if err != nil || len(exts) == 0 {
		switch mediaType {
		case "text/plain":
			return ".txt"
		case "text/html":
			return ".html"
		case "application/json":
			return ".json"
		case "application/pdf":
			return ".pdf"
		default:
			return ".bin"
		}
	}
	return exts[0]
}

// parseID extracts and validates the :id path parameter.
func parseID(c *gin.Context) (int64, bool) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid message id"})
		return 0, false
	}
	return id, true
}

// isRecipient checks whether addr appears in the to list (case-insensitive).
func isRecipient(to []string, addr string) bool {
	for _, a := range to {
		if strings.EqualFold(a, addr) {
			return true
		}
	}
	return false
}

// isZip reports whether data starts with the zip local file header signature.
func isZip(data []byte) bool {
	return len(data) >= 4 && data[0] == 0x50 && data[1] == 0x4b && data[2] == 0x03 && data[3] == 0x04
}

// checkDistinctRecipients returns an error if any address in to or addTo
// appears more than once (case-insensitive).
func checkDistinctRecipients(to, addTo []string) error {
	seen := make(map[string]struct{}, len(to))
	for _, addr := range to {
		key := strings.ToLower(addr)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("duplicate recipient: %s", addr)
		}
		seen[key] = struct{}{}
	}
	seenAddTo := make(map[string]struct{}, len(addTo))
	for _, addr := range addTo {
		key := strings.ToLower(addr)
		if _, dup := seenAddTo[key]; dup {
			return fmt.Errorf("duplicate recipient: %s", addr)
		}
		seenAddTo[key] = struct{}{}
	}
	return nil
}
