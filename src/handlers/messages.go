// Package handlers implements HTTP handlers for the fmsg web API.
package handlers

import (
"context"
"crypto/sha256"
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
DB      *db.DB
DataDir string
}

// NewMessageHandler creates a MessageHandler.
func NewMessageHandler(database *db.DB, dataDir string) *MessageHandler {
return &MessageHandler{DB: database, DataDir: dataDir}
}

// Create handles POST /api/v1/messages — creates a draft message.
func (h *MessageHandler) Create(c *gin.Context) {
identity := middleware.GetIdentity(c)
if identity == "" {
c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
return
}

var msg models.Message
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

// Compute SHA-256 of the data payload.
hash := sha256.Sum256([]byte(msg.Data))

// Parse extension from MIME type.
ext := mimeToExt(msg.Type)

ctx := c.Request.Context()

// Insert message row with empty filepath; update after we know the ID.
var msgID int64
err := h.DB.Pool.QueryRow(ctx,
`INSERT INTO msg (version, pid, flags, from_addr, topic, type, sha256, size, filepath)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, '')
 RETURNING id`,
msg.Version, msg.PID, msg.Flags, msg.From, msg.Topic, msg.Type, hash[:], msg.Size,
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

// Authorization: owner or recipient.
if msg.From != identity && !isRecipient(msg.To, identity) {
c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
return
}

c.JSON(http.StatusOK, msg)
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

var msg models.Message
if err := c.ShouldBindJSON(&msg); err != nil {
c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
return
}
if msg.From != identity {
c.JSON(http.StatusForbidden, gin.H{"error": "from address must match authenticated user"})
return
}

hash := sha256.Sum256([]byte(msg.Data))
ext := mimeToExt(msg.Type)

dataPath, err := h.saveMessageData(msg.From, msgID, ext, msg.Data)
if err != nil {
log.Printf("update message %d save: %v", msgID, err)
c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save message data"})
return
}

_, err = h.DB.Pool.Exec(ctx,
`UPDATE msg SET version=$1, pid=$2, flags=$3, topic=$4, type=$5, sha256=$6, size=$7, filepath=$8 WHERE id=$9`,
msg.Version, msg.PID, msg.Flags, msg.Topic, msg.Type, hash[:], msg.Size, dataPath, msgID,
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

// fetchMessage loads a message with its recipients and attachments from the DB.
func (h *MessageHandler) fetchMessage(ctx context.Context, msgID int64) (*models.Message, error) {
row := h.DB.Pool.QueryRow(ctx,
`SELECT version, pid, flags, time_sent, from_addr, topic, type, size FROM msg WHERE id = $1`,
msgID,
)

msg := &models.Message{}
var pid *int64
var timeSent *float64
if err := row.Scan(&msg.Version, &pid, &msg.Flags, &timeSent, &msg.From, &msg.Topic, &msg.Type, &msg.Size); err != nil {
return nil, err
}
msg.PID = pid
msg.Time = timeSent

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

// isRecipient checks whether addr appears in the to list.
func isRecipient(to []string, addr string) bool {
for _, a := range to {
if a == addr {
return true
}
}
return false
}
