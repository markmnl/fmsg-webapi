package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/internal/emoji"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// FMSG-006 indicators: a participant's short-lived, single-emoji state on a
// message, such as composing a reply. Nothing is stored. An indicator is
// published on the PostgreSQL NOTIFY channel "indicator"; the WebSocket hub
// of every instance delivers it to the message's connected participants, and
// the host (fmsgd) sends it on to the participants' other hosts.

const (
	// IndicatorChannel is the NOTIFY channel indicators travel on, between
	// this service and the host, in both directions.
	IndicatorChannel = "indicator"
	// indicatorTTLMax is the longest ttl accepted (FMSG-006 INDICATOR_TTL_MAX).
	indicatorTTLMax = 120
	// indicatorTTLDefault is the ttl when a request leaves it out.
	indicatorTTLDefault = 6
	// indicatorMinInterval is the shortest time between indicators one
	// address sets on one message; more frequent ones are dropped.
	indicatorMinInterval = time.Second
)

// Indicator is the NOTIFY payload and, with RootID, the WebSocket event body.
// ID is the subject message; From the indicating participant; an empty Emoji
// or a zero TTL clears the state.
type Indicator struct {
	ID     int64  `json:"id"`
	RootID int64  `json:"root_id,omitempty"`
	From   string `json:"from"`
	Emoji  string `json:"emoji"`
	TTL    int    `json:"ttl"`
}

type indicateInput struct {
	Emoji *string `json:"emoji"`
	TTL   *int    `json:"ttl"`
}

// indicatorThrottle drops indicators set more often than indicatorMinInterval
// for one address on one message. Clears are never throttled.
type indicatorThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var throttle = &indicatorThrottle{last: map[string]time.Time{}}

func (t *indicatorThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if prev, ok := t.last[key]; ok && now.Sub(prev) < indicatorMinInterval {
		return false
	}
	if len(t.last) > 10000 {
		for k, v := range t.last {
			if now.Sub(v) >= indicatorMinInterval {
				delete(t.last, k)
			}
		}
	}
	t.last[key] = now
	return true
}

// Indicate handles POST /fmsg/:id/indicate {"emoji", "ttl"}: sets or clears
// the caller's indicator on a message they are a participant of. It answers
// 202 Accepted with no body; an indicator is best effort.
func (h *MessageHandler) Indicate(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := resolveID(c, h.q())
	if !ok {
		return
	}
	var in indicateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ind := Indicator{ID: msgID, From: identity, TTL: indicatorTTLDefault}
	if in.Emoji != nil {
		ind.Emoji = *in.Emoji
	}
	if in.TTL != nil {
		if *in.TTL < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "ttl must be 0 to clear, or 1 to 120 seconds"})
			return
		}
		ind.TTL = min(*in.TTL, indicatorTTLMax)
	}
	if ind.Emoji != "" && !emoji.IsSingle(ind.Emoji) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji must be a single emoji, or empty to clear"})
		return
	}
	if ind.Emoji == "" || ind.TTL == 0 {
		ind.Emoji, ind.TTL = "", 0
	}

	ctx := c.Request.Context()
	subject, _, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("indicate on %d: fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}
	if draftHidden([]string{identity}, subject.From, subject.Time) || purgedFor(ctx, h.q(), identity, msgID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if !strings.EqualFold(subject.From, identity) && !isRecipient(subject.To, identity) && !addToContains(subject.AddTo, identity) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only participants may show an indicator on a message"})
		return
	}
	if subject.Time == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot show an indicator on a draft"})
		return
	}
	if subject.Terminal {
		c.JSON(http.StatusConflict, gin.H{"error": "message is terminal; it cannot be replied to"})
		return
	}

	if ind.Emoji != "" && !throttle.allow(strings.ToLower(identity)+"|"+strconv.FormatInt(msgID, 10), time.Now()) {
		c.Status(http.StatusAccepted)
		return
	}
	payload, err := json.Marshal(ind)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to publish indicator"})
		return
	}
	if _, err := h.q().Exec(ctx, `SELECT pg_notify($1, $2)`, IndicatorChannel, string(payload)); err != nil {
		log.Printf("indicate on %d: notify: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to publish indicator"})
		return
	}
	c.Status(http.StatusAccepted)
}

// indicatorAudience returns every participant of message msgID (its sender,
// recipients, and each add-to batch's adder and added recipients) and the id
// of its thread's first message.
func (h *MessageHandler) indicatorAudience(ctx context.Context, msgID int64) ([]string, int64, error) {
	rows, err := h.q().Query(ctx, `
		SELECT from_addr FROM msg WHERE id = $1
		UNION SELECT addr FROM msg_to WHERE msg_id = $1
		UNION SELECT add_to_from FROM msg_add_to_batch WHERE msg_id = $1
		UNION SELECT addr FROM msg_add_to WHERE msg_id = $1`, msgID)
	if err != nil {
		return nil, 0, err
	}
	var addrs []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, 0, err
		}
		addrs = append(addrs, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var root int64
	err = h.q().QueryRow(ctx, `
		WITH RECURSIVE chain AS (
		    SELECT id, pid, 0 AS depth FROM msg WHERE id = $1
		    UNION ALL
		    SELECT m.id, m.pid, c.depth + 1 FROM msg m JOIN chain c ON m.id = c.pid WHERE c.depth < 10000
		)
		SELECT id FROM chain ORDER BY depth DESC LIMIT 1`, msgID).Scan(&root)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, err
	}
	if root == 0 {
		root = msgID
	}
	return addrs, root, nil
}
