package handlers

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/db"
	"github.com/markmnl/fmsg-webapi/middleware"
)

// AttachmentHandler holds dependencies for attachment routes.
type AttachmentHandler struct {
	DB      *db.DB
	DataDir string
}

// NewAttachmentHandler creates an AttachmentHandler.
func NewAttachmentHandler(database *db.DB, dataDir string) *AttachmentHandler {
	return &AttachmentHandler{DB: database, DataDir: dataDir}
}

// Upload handles POST /api/v1/messages/:id/attachments.
func (h *AttachmentHandler) Upload(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()

	// Load the message to check ownership and draft status.
	var fromAddr string
	var timeSent *float64
	err := h.DB.Pool.QueryRow(ctx,
		"SELECT from_addr, time_sent FROM msg WHERE id = $1", msgID,
	).Scan(&fromAddr, &timeSent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("upload attachment: fetch msg %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	if fromAddr != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner may upload attachments"})
		return
	}
	if timeSent != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "attachments cannot be added to a sent message"})
		return
	}

	// Expect multipart file upload with field "file".
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file field required"})
		return
	}
	defer file.Close()

	// Sanitize the intended filename (no path components).
	intendedFilename := filepath.Base(header.Filename)
	if intendedFilename == "." || intendedFilename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}

	// Determine directory for this message.
	dir := msgDataDir(h.DataDir, fromAddr, msgID)
	if err = os.MkdirAll(dir, 0750); err != nil {
		log.Printf("upload attachment: mkdir: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare storage"})
		return
	}

	// Resolve collision-safe filepath.
	finalPath := resolveFilePath(dir, intendedFilename)

	// Write file to disk.
	dst, err := os.OpenFile(finalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		log.Printf("upload attachment: open file: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save attachment"})
		return
	}
	written, err := io.Copy(dst, file)
	closeErr := dst.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(finalPath)
		log.Printf("upload attachment: write: %v / %v", err, closeErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write attachment"})
		return
	}

	// Persist to DB.
	_, err = h.DB.Pool.Exec(ctx,
		`INSERT INTO msg_attachment (msg_id, filename, filesize, filepath)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (msg_id, filename) DO UPDATE SET filesize=$3, filepath=$4`,
		msgID, intendedFilename, written, finalPath,
	)
	if err != nil {
		_ = os.Remove(finalPath)
		log.Printf("upload attachment: insert: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record attachment"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"filename": intendedFilename, "size": written})
}

// Download handles GET /api/v1/messages/:id/attachments/:filename.
func (h *AttachmentHandler) Download(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	// Validate and sanitize filename parameter.
	filename := filepath.Base(c.Param("filename"))
	if filename == "." || filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}

	ctx := c.Request.Context()

	// Check ownership or recipient access.
	var fromAddr string
	err := h.DB.Pool.QueryRow(ctx, "SELECT from_addr FROM msg WHERE id = $1", msgID).Scan(&fromAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("download attachment: fetch msg %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	// Check recipients (to or add_to) if not owner.
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

	// Look up the attachment filepath.
	var storedPath string
	err = h.DB.Pool.QueryRow(ctx,
		"SELECT filepath FROM msg_attachment WHERE msg_id = $1 AND filename = $2",
		msgID, filename,
	).Scan(&storedPath)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		} else {
			log.Printf("download attachment: fetch path: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve attachment"})
		}
		return
	}

	// Path traversal protection: ensure the stored path is within DataDir.
	cleanPath := filepath.Clean(storedPath)
	cleanDataDir := filepath.Clean(h.DataDir)
	if !strings.HasPrefix(cleanPath, cleanDataDir+string(filepath.Separator)) {
		log.Printf("download attachment: path traversal attempt: %s", storedPath)
		c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
		return
	}

	c.FileAttachment(cleanPath, filename)
}

// DeleteAttachment handles DELETE /api/v1/messages/:id/attachments/:filename.
func (h *AttachmentHandler) DeleteAttachment(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := parseID(c)
	if !ok {
		return
	}

	filename := filepath.Base(c.Param("filename"))
	if filename == "." || filename == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}

	ctx := c.Request.Context()

	var fromAddr string
	var timeSent *float64
	err := h.DB.Pool.QueryRow(ctx,
		"SELECT from_addr, time_sent FROM msg WHERE id = $1", msgID,
	).Scan(&fromAddr, &timeSent)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("delete attachment: fetch msg %d: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	if fromAddr != identity {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the owner may delete attachments"})
		return
	}
	if timeSent != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "attachments of a sent message cannot be deleted"})
		return
	}

	// Get filepath before deleting.
	var storedPath string
	err = h.DB.Pool.QueryRow(ctx,
		"SELECT filepath FROM msg_attachment WHERE msg_id = $1 AND filename = $2",
		msgID, filename,
	).Scan(&storedPath)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "attachment not found"})
		} else {
			log.Printf("delete attachment: fetch path: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve attachment"})
		}
		return
	}

	if _, err = h.DB.Pool.Exec(ctx,
		"DELETE FROM msg_attachment WHERE msg_id = $1 AND filename = $2", msgID, filename,
	); err != nil {
		log.Printf("delete attachment: db: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete attachment"})
		return
	}

	// Remove file from disk.
	cleanPath := filepath.Clean(storedPath)
	cleanDataDir := filepath.Clean(h.DataDir)
	if strings.HasPrefix(cleanPath, cleanDataDir+string(filepath.Separator)) {
		_ = os.Remove(cleanPath)
	}

	c.Status(http.StatusNoContent)
}

// resolveFilePath returns a path under dir for filename, incrementing a suffix
// until no collision is found.
func resolveFilePath(dir, filename string) string {
	candidate := filepath.Join(dir, filename)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 1; ; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
