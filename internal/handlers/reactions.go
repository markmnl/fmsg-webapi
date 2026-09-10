package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/markmnl/fmsgd/pkg/message"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/markmnl/fmsg-webapi/internal/emoji"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
	"github.com/markmnl/fmsg-webapi/internal/models"
)

// FMSG-005 Reactions. A reaction is an ordinary message recognised by its
// shape: a reply (pid set) with no reply and terminal set, not important, with
// no add-to batch, body type text/plain;charset=UTF-8 (Common Media Type 56),
// no attachments, and data that is a single emoji or empty to clear. A
// reactor's effective reaction on a subject is their latest reaction message
// by time, ties broken by the greater message hash.

const (
	// reactionMediaType is Common Media Type 56, the body type of a reaction.
	reactionMediaType = "text/plain;charset=UTF-8"
	// maxReactionBytes bounds reaction data (FMSG-005 Fields).
	maxReactionBytes = 64
)

// isReactionMediaType reports whether a stored type string is the reaction
// media type, tolerating case and whitespace differences in the parameter.
func isReactionMediaType(mediaType string) bool {
	return strings.EqualFold(strings.ReplaceAll(mediaType, " ", ""), reactionMediaType)
}

// reactionShape reports whether a message's metadata has the shape of a
// reaction, before its data is examined.
func reactionShape(hasPid, noReply, terminal, important, hasAddTo bool, mediaType string, size, attachments int) bool {
	return hasPid && noReply && terminal && !important && !hasAddTo &&
		size >= 0 && size <= maxReactionBytes && attachments == 0 &&
		isReactionMediaType(mediaType)
}

// readReactionData reads a shape-matching message's body and returns the emoji
// it carries ("" clears) and whether the body is a valid reaction body.
func (h *MessageHandler) readReactionData(dataPath string, size int) (string, bool) {
	if size == 0 {
		return "", true
	}
	cleanPath, ok := safeDataPath(dataPath, h.DataDir)
	if !ok {
		return "", false
	}
	f, err := os.Open(cleanPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("reaction data: open %s: %v", cleanPath, err)
		}
		return "", false
	}
	defer f.Close()
	buf := make([]byte, maxReactionBytes+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		log.Printf("reaction data: read %s: %v", cleanPath, err)
		return "", false
	}
	s := string(buf[:n])
	if !emoji.IsSingle(s) {
		return "", false
	}
	return s, true
}

// reactionOf returns the reaction a message carries, or nil when the message
// is not a reaction.
func (h *MessageHandler) reactionOf(hasPid, noReply, terminal, important, hasAddTo bool, mediaType string, size, attachments int, dataPath string) *string {
	if !reactionShape(hasPid, noReply, terminal, important, hasAddTo, mediaType, size, attachments) {
		return nil
	}
	s, ok := h.readReactionData(dataPath, size)
	if !ok {
		return nil
	}
	return &s
}

// reactionSubject reports the subject of msgID when msgID is a reaction. Used
// by the WebSocket hub to route an arriving reaction as a reaction event.
func (h *MessageHandler) reactionSubject(ctx context.Context, msgID int64) (int64, bool) {
	var pid *int64
	var noReply, terminal, important, hasAddTo bool
	var mediaType, dataPath string
	var size, attachments int
	err := h.q().QueryRow(ctx, `
		SELECT m.pid, m.no_reply, m.is_terminal, m.is_important, m.type, m.size, m.filepath,
		       (SELECT COUNT(*) FROM msg_attachment a WHERE a.msg_id = m.id),
		       EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id = m.id)
		FROM msg m WHERE m.id = $1`, msgID,
	).Scan(&pid, &noReply, &terminal, &important, &mediaType, &size, &dataPath, &attachments, &hasAddTo)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("reaction subject of %d: %v", msgID, err)
		}
		return 0, false
	}
	if pid == nil || h.reactionOf(true, noReply, terminal, important, hasAddTo, mediaType, size, attachments, dataPath) == nil {
		return 0, false
	}
	return *pid, true
}

// reactionRow is one stored reaction message on a subject.
type reactionRow struct {
	subjectID int64
	msgID     int64
	from      string
	time      float64
	hash      []byte // message hash; nil until the host has computed it
	emoji     string // "" clears
}

// newerReaction reports whether a supersedes b as a reactor's effective
// reaction: later time wins, ties broken by the greater message hash.
func newerReaction(a, b reactionRow) bool {
	if a.time != b.time {
		return a.time > b.time
	}
	return bytes.Compare(a.hash, b.hash) > 0
}

// effectiveReactions reduces reaction rows to each subject's effective
// reactions: one per reactor, grouped by emoji in order of first reaction,
// reactors within a group in time order. A reactor whose effective reaction is
// empty has none and is omitted.
func effectiveReactions(rows []reactionRow) map[int64][]models.Reaction {
	bySubject := make(map[int64]map[string]reactionRow)
	for _, r := range rows {
		reactors := bySubject[r.subjectID]
		if reactors == nil {
			reactors = make(map[string]reactionRow)
			bySubject[r.subjectID] = reactors
		}
		key := strings.ToLower(r.from)
		if cur, ok := reactors[key]; !ok || newerReaction(r, cur) {
			reactors[key] = r
		}
	}

	result := make(map[int64][]models.Reaction, len(bySubject))
	for subject, reactors := range bySubject {
		var eff []reactionRow
		for _, r := range reactors {
			if r.emoji != "" {
				eff = append(eff, r)
			}
		}
		sort.Slice(eff, func(i, j int) bool {
			if eff[i].time != eff[j].time {
				return eff[i].time < eff[j].time
			}
			return eff[i].msgID < eff[j].msgID
		})
		var groups []models.Reaction
		idx := make(map[string]int)
		for _, r := range eff {
			i, ok := idx[r.emoji]
			if !ok {
				groups = append(groups, models.Reaction{Emoji: r.emoji})
				i = len(groups) - 1
				idx[r.emoji] = i
			}
			groups[i].From = append(groups[i].From, r.from)
		}
		if len(groups) > 0 {
			result[subject] = groups
		}
	}
	return result
}

// loadReactionRows loads every sent reaction on the given subjects, or only
// those from one reactor when from is non-empty. Rows whose data is not a
// valid reaction body are ordinary messages and are skipped.
func (h *MessageHandler) loadReactionRows(ctx context.Context, subjectIDs []int64, from string) ([]reactionRow, error) {
	if len(subjectIDs) == 0 {
		return nil, nil
	}
	rows, err := h.q().Query(ctx, `
		SELECT r.pid, r.id, r.from_addr, r.time_sent, r.sha256, r.size, r.filepath
		FROM msg r
		WHERE r.pid = ANY($1)
		  AND r.time_sent IS NOT NULL
		  AND r.is_terminal AND r.no_reply AND NOT r.is_important
		  AND r.size <= $2
		  AND lower(replace(r.type, ' ', '')) = $3
		  AND ($4 = '' OR lower(r.from_addr) = lower($4))
		  AND NOT EXISTS (SELECT 1 FROM msg_attachment a WHERE a.msg_id = r.id)
		  AND NOT EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id = r.id)
		ORDER BY r.pid, r.time_sent, r.id`,
		subjectIDs, maxReactionBytes, strings.ToLower(reactionMediaType), from,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reactionRow
	for rows.Next() {
		var r reactionRow
		var size int
		var dataPath string
		if err := rows.Scan(&r.subjectID, &r.msgID, &r.from, &r.time, &r.hash, &size, &dataPath); err != nil {
			return nil, err
		}
		e, ok := h.readReactionData(dataPath, size)
		if !ok {
			continue
		}
		r.emoji = e
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadReactions returns the effective reactions on each of the given subjects.
func (h *MessageHandler) loadReactions(ctx context.Context, subjectIDs []int64) map[int64][]models.Reaction {
	rows, err := h.loadReactionRows(ctx, subjectIDs, "")
	if err != nil {
		log.Printf("load reactions: %v", err)
		return nil
	}
	return effectiveReactions(rows)
}

// populateListReactions fills Reaction and Reactions on every list item.
func (h *MessageHandler) populateListReactions(ctx context.Context, items []messageListItem) {
	ids := make([]int64, len(items))
	for i := range items {
		it := &items[i]
		it.Reaction = h.reactionOf(it.HasPid, it.NoReply, it.Terminal, it.Important, it.HasAddTo, it.Type, it.Size, len(it.Attachments), it.dataPath)
		ids[i] = it.ID
	}
	reactions := h.loadReactions(ctx, ids)
	for i := range items {
		items[i].Reactions = reactions[items[i].ID]
		if items[i].Reactions == nil {
			items[i].Reactions = []models.Reaction{}
		}
	}
}

// reactionRecipients returns a subject's participants other than the reactor,
// in message order and deduplicated case-insensitively: the to list of a
// reaction (FMSG-005 Recipients).
func reactionRecipients(subject *models.Message, reactor string) []string {
	seen := map[string]bool{strings.ToLower(reactor): true}
	var out []string
	add := func(addr string) {
		key := strings.ToLower(addr)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, addr)
	}
	add(subject.From)
	for _, a := range subject.To {
		add(a)
	}
	for _, b := range subject.AddTo {
		add(b.AddToFrom)
		for _, a := range b.To {
			add(a)
		}
	}
	return out
}

// reactInput is the request body of POST /fmsg/:id/react.
type reactInput struct {
	Emoji *string `json:"emoji"` // a single emoji; null or "" clears the caller's reaction
}

// React handles POST /fmsg/:id/react — sets or clears the caller's reaction on
// a message by sending a reaction message (FMSG-005) to every other
// participant. Sending an identical reaction to the current effective one is
// idempotent and sends nothing.
func (h *MessageHandler) React(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := resolveID(c, h.q())
	if !ok {
		return
	}

	var in reactInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	want := ""
	if in.Emoji != nil {
		want = *in.Emoji
	}
	if want != "" && !emoji.IsSingle(want) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "emoji must be a single emoji, or empty to clear"})
		return
	}

	ctx := c.Request.Context()
	subject, _, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("react to %d: fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}

	// Only a participant of the subject may react to it (the host enforces
	// the same rule on the wire).
	if !strings.EqualFold(subject.From, identity) && !isRecipient(subject.To, identity) && !addToContains(subject.AddTo, identity) {
		c.JSON(http.StatusForbidden, gin.H{"error": "only participants may react to a message"})
		return
	}
	if subject.Time == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot react to a draft"})
		return
	}
	if subject.Terminal {
		c.JSON(http.StatusConflict, gin.H{"error": "message is terminal; it cannot be reacted to"})
		return
	}

	recipients := reactionRecipients(subject, identity)
	if len(recipients) == 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "message has no other participants to send a reaction to"})
		return
	}

	// Idempotency: the caller's current effective reaction.
	mine, err := h.loadReactionRows(ctx, []int64{msgID}, identity)
	if err != nil {
		log.Printf("react to %d: load reactions: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load reactions"})
		return
	}
	var current *reactionRow
	for i := range mine {
		if current == nil || newerReaction(mine[i], *current) {
			current = &mine[i]
		}
	}
	switch {
	case current != nil && current.emoji == want:
		c.JSON(http.StatusOK, gin.H{"id": current.msgID, "time": current.time, "sha256": hex.EncodeToString(current.hash)})
		return
	case current == nil && want == "":
		c.JSON(http.StatusOK, gin.H{"id": nil, "time": nil, "sha256": nil})
		return
	}

	// As for any reply, refuse when a remote recipient host can never accept
	// it because it does not hold the subject (SPEC §10.3, code 6).
	if replyDomains := remoteRecipientDomains(&models.Message{To: recipients}, h.LocalDomain); len(replyDomains) > 0 {
		parentFromDomain, byDomain, derr := h.parentDeliveryByDomain(ctx, msgID)
		if derr != nil {
			log.Printf("react to %d: verify delivery: %v", msgID, derr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify parent delivery"})
			return
		}
		if blocked := undeliverableReplyDomains(replyDomains, parentFromDomain, byDomain); len(blocked) > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "reaction cannot be accepted by recipient host(s): " + strings.Join(blocked, "; ")})
			return
		}
	}

	parentHash := subject.SHA256
	if !sameAddr(subject.From, identity) && !isRecipient(subject.To, identity) {
		parentHash = nil
		for _, b := range subject.AddTo {
			if isRecipient(b.To, identity) {
				parentHash = b.SHA256
				break
			}
		}
	}
	if parentHash == nil {
		c.JSON(409, gin.H{"error": "reaction parent requires hash backfill"})
		return
	}
	now := float64(time.Now().UnixMicro()) / 1e6
	tx, err := h.q().Begin(ctx)
	if err != nil {
		log.Printf("react to %d: begin: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send reaction"})
		return
	}
	defer tx.Rollback(ctx)

	var reactionID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO msg (version, pid, psha256, no_reply, is_important, is_deflate, is_terminal, from_addr, topic, type, size, filepath, time_sent)
		 VALUES (1, $1, decode($5,'hex'), true, false, false, true, $2, '', $3, $4, '', NULL)
		 RETURNING id`,
		msgID, identity, reactionMediaType, len(want), parentHash,
	).Scan(&reactionID); err != nil {
		log.Printf("react to %d: insert: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send reaction"})
		return
	}
	dataPath, err := h.saveMessageData(identity, reactionID, mimeToExt(reactionMediaType), want)
	if err != nil {
		log.Printf("react to %d: save data: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save reaction"})
		return
	}
	createdFile(c, dataPath)
	if _, err := tx.Exec(ctx, "UPDATE msg SET filepath = $1 WHERE id = $2", dataPath, reactionID); err != nil {
		log.Printf("react to %d: update filepath: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send reaction"})
		return
	}
	for _, addr := range recipients {
		if _, err := tx.Exec(ctx, "INSERT INTO msg_to (msg_id, addr) VALUES ($1, $2)", reactionID, addr); err != nil {
			log.Printf("react to %d: insert recipient %s: %v", msgID, addr, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send reaction"})
			return
		}
	}
	var files message.Files
	onRollback(c, func() { files.Cleanup() })
	hash, err := message.Finalize(ctx, finalizationTx{tx}, reactionID, now, &files)
	if err != nil {
		log.Printf("finalize reaction: %v", err)
		c.JSON(500, gin.H{"error": "failed to finalize reaction"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("react to %d: commit: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send reaction"})
		return
	}

	// fmsgd's outbound sender skips the local domain, so local recipients
	// have their delivery resolved here, as Send does.
	afterCommit(c, func() {
		plain := *h
		plain.query = nil
		plain.resolveLocalDelivery(ctx, "msg_to", reactionID, h.LocalDomain, recipients)
	})

	c.JSON(http.StatusCreated, gin.H{"id": reactionID, "time": now, "sha256": hex.EncodeToString(hash)})
}
