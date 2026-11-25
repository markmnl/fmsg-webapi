package handlers

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

const resolvedMsgIDKey = "resolvedMsgID"

type cachedResolvedID struct {
	raw string
	id  int64
}

func decodeHash(s string) ([]byte, error) {
	if len(s) != 64 {
		return nil, errors.New("hash must contain 64 hexadecimal characters")
	}
	return hex.DecodeString(s)
}

func resolveID(c *gin.Context, q queries) (int64, bool) {
	s := c.Param("id")
	if v, ok := c.Get(resolvedMsgIDKey); ok {
		if cached, ok := v.(cachedResolvedID); ok && cached.raw == s {
			return cached.id, true
		}
	}
	if len(s) != 64 {
		id, ok := parseID(c)
		if ok {
			c.Set(resolvedMsgIDKey, cachedResolvedID{raw: s, id: id})
		}
		return id, ok
	}
	hash, err := decodeHash(s)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid message sha256"})
		return 0, false
	}
	var id int64
	err = q.QueryRow(c.Request.Context(), `SELECT id FROM msg WHERE sha256=$1`, hash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return 0, false
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "failed to resolve message"})
		return 0, false
	}
	c.Set(resolvedMsgIDKey, cachedResolvedID{raw: s, id: id})
	return id, true
}

// Resolve request pid separately from response pid, preserving the numeric
// response contract and the exact parent hash when it identifies a batch.
func (h *MessageHandler) resolveParent(c *gin.Context, in *messageInput) bool {
	in.PID = nil
	in.PSHA256 = nil
	in.SHA256 = nil
	raw := bytes.TrimSpace(in.Parent)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	var id int64
	var hash []byte
	var err error
	if raw[0] == '"' {
		var value string
		err = json.Unmarshal(raw, &value)
		if err == nil {
			hash, err = decodeHash(value)
		}
	} else {
		id, err = strconv.ParseInt(string(raw), 10, 64)
		if id <= 0 {
			err = errors.New("parent id must be positive")
		}
	}
	if err != nil {
		c.JSON(400, gin.H{"error": "pid must be a positive integer or a 64-character sha256 string"})
		return false
	}
	ctx := c.Request.Context()
	var canonical []byte
	var sent *float64
	var terminal bool
	var batchID *int64
	if hash != nil {
		err = h.q().QueryRow(ctx, `SELECT id, sha256, time_sent, is_terminal, batch_id
		 FROM (
		     SELECT m.id, m.sha256, m.time_sent, m.is_terminal, NULL::bigint AS batch_id
		       FROM msg m
		      WHERE m.sha256 = $1
		     UNION ALL
		     SELECT m.id, m.sha256, m.time_sent, m.is_terminal, b.id AS batch_id
		       FROM msg_add_to_batch b
		       JOIN msg m ON m.id = b.msg_id
		      WHERE b.sha256 = $1
		 ) p
		 ORDER BY batch_id NULLS FIRST
		 LIMIT 1`, hash).Scan(&id, &canonical, &sent, &terminal, &batchID)
	} else {
		err = h.q().QueryRow(ctx, `SELECT sha256,time_sent,is_terminal FROM msg WHERE id=$1`, id).Scan(&canonical, &sent, &terminal)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		c.JSON(400, gin.H{"error": "parent message not found"})
		return false
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "failed to resolve parent"})
		return false
	}
	var participant bool
	err = h.q().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM msg WHERE id=$1 AND lower(from_addr)=lower($2))
	 OR EXISTS(SELECT 1 FROM msg_to WHERE msg_id=$1 AND lower(addr)=lower($2))
	 OR EXISTS(SELECT 1 FROM msg_add_to WHERE msg_id=$1 AND batch_id=$3 AND lower(addr)=lower($2))`, id, middleware.GetIdentity(c), batchID).Scan(&participant)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed to validate parent participation"})
		return false
	}
	if !participant {
		c.JSON(403, gin.H{"error": "not a participant of this parent; added recipients must use their batch sha256"})
		return false
	}
	// A parent deleted for good no longer exists for the caller.
	purged, err := purgedFor(ctx, h.q(), middleware.GetIdentity(c), id)
	if err != nil {
		log.Printf("resolve parent %d: trash state: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to validate parent"})
		return false
	}
	if purged {
		c.JSON(400, gin.H{"error": "parent message not found"})
		return false
	}
	if sent == nil || terminal {
		c.JSON(409, gin.H{"error": "parent is a draft or terminal message"})
		return false
	}
	if hash == nil {
		hash = canonical
	}
	if len(hash) != 32 {
		c.JSON(500, gin.H{"error": "parent has no finalized identity"})
		return false
	}
	encoded := hex.EncodeToString(hash)
	in.PID = &id
	in.PSHA256 = &encoded
	return true
}
