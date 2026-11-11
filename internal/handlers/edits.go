package handlers

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/markmnl/fmsg-webapi/internal/edit"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
	"github.com/markmnl/fmsg-webapi/internal/models"
	"github.com/markmnl/fmsgd/pkg/message"
)

// FMSG-008 Edits. An edit is an ordinary message: a reply (pid set) from the
// subject's author with no reply and terminal set, not important, with no
// add-to batch and no attachments, of type application/vnd.fmsg.edit, whose
// data is empty (revert) or a well-formed edit document for the subject's
// data. A subject's effective edit is its latest edit message by time, ties
// broken by the greater message hash. The server applies it for clients:
// short_text and the data route present the edited data.

// isEditMediaType reports whether a stored type string is the edit media type.
func isEditMediaType(mediaType string) bool {
	return strings.EqualFold(strings.TrimSpace(mediaType), edit.MediaType)
}

// editShape reports whether a message's metadata has the shape of an edit,
// before its author and data are examined.
func editShape(hasPid, noReply, terminal, important, hasAddTo bool, mediaType string, attachments int) bool {
	return hasPid && noReply && terminal && !important && !hasAddTo &&
		attachments == 0 && isEditMediaType(mediaType)
}

// readBody reads a stored message body. An empty path is an empty body.
func (h *MessageHandler) readBody(dataPath string) ([]byte, bool) {
	if dataPath == "" {
		return nil, true
	}
	cleanPath, ok := safeDataPath(dataPath, h.DataDir)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(cleanPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("edit: read %s: %v", cleanPath, err)
		}
		return nil, false
	}
	return data, true
}

// editLimit bounds the size of edited data this server will produce for a
// subject (FMSG-008 Applying Edits).
func (h *MessageHandler) editLimit(subjectLen int) int64 {
	return h.MaxDataSize + int64(subjectLen)
}

// editSubject reports the subject of msgID when msgID is an edit message.
// Used to flag edit messages and by the WebSocket hub to route an arriving
// edit as an edit event.
func (h *MessageHandler) editSubject(ctx context.Context, msgID int64) (int64, bool) {
	var pid int64
	var size int
	var dataPath, subjectPath string
	err := h.q().QueryRow(ctx, `
		SELECT r.pid, r.size, r.filepath, s.filepath
		FROM msg r JOIN msg s ON s.id = r.pid
		WHERE r.id = $1 AND `+editRowFilter, msgID, edit.MediaType,
	).Scan(&pid, &size, &dataPath, &subjectPath)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("edit subject of %d: %v", msgID, err)
		}
		return 0, false
	}
	if size == 0 {
		return pid, true
	}
	subject, ok := h.readBody(subjectPath)
	if !ok {
		return 0, false
	}
	doc, ok := h.readBody(dataPath)
	if !ok {
		return 0, false
	}
	if _, err := edit.Check(doc, len(subject)); err != nil {
		return 0, false
	}
	return pid, true
}

// editRowFilter selects sent messages r having the shape of an edit of their
// parent s, written by s's author. $2 is the edit media type.
const editRowFilter = `r.time_sent IS NOT NULL
		  AND r.is_terminal AND r.no_reply AND NOT r.is_important
		  AND lower(btrim(r.type)) = $2
		  AND lower(r.from_addr) = lower(s.from_addr)
		  AND NOT EXISTS (SELECT 1 FROM msg_attachment a WHERE a.msg_id = r.id)
		  AND NOT EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id = r.id)`

// editRow is one stored edit-shaped message on a subject.
type editRow struct {
	msgID    int64
	time     float64
	hash     []byte // message hash; nil until the host has computed it
	size     int
	dataPath string
}

// effectiveEdit is the edit in force on a subject.
type effectiveEdit struct {
	msgID   int64
	time    float64
	hash    []byte
	revert  bool   // the latest edit reverts to the original
	applied bool   // data holds the edited data
	data    []byte // edited data, when applied
}

// edited returns the API representation, or nil when the subject presents its
// original data because the latest edit reverts.
func (e *effectiveEdit) edited() *models.Edited {
	if e == nil || e.revert {
		return nil
	}
	out := &models.Edited{ID: e.msgID, Time: e.time}
	if e.applied {
		n := len(e.data)
		out.Size = &n
	}
	return out
}

// loadEffectiveEdits returns the effective edit of each of the given subjects
// that has one. Edit-shaped messages whose data is not a well-formed edit
// document for the subject are ordinary messages and are skipped.
func (h *MessageHandler) loadEffectiveEdits(ctx context.Context, subjectIDs []int64) map[int64]*effectiveEdit {
	if len(subjectIDs) == 0 {
		return nil
	}
	rows, err := h.q().Query(ctx, `
		SELECT r.pid, r.id, r.time_sent, r.sha256, r.size, r.filepath, s.filepath
		FROM msg r JOIN msg s ON s.id = r.pid
		WHERE r.pid = ANY($1) AND `+editRowFilter,
		subjectIDs, edit.MediaType,
	)
	if err != nil {
		log.Printf("load edits: %v", err)
		return nil
	}
	bySubject := make(map[int64][]editRow)
	subjectPaths := make(map[int64]string)
	for rows.Next() {
		var subjectID int64
		var r editRow
		var subjectPath string
		if err := rows.Scan(&subjectID, &r.msgID, &r.time, &r.hash, &r.size, &r.dataPath, &subjectPath); err != nil {
			rows.Close()
			log.Printf("load edits: scan: %v", err)
			return nil
		}
		bySubject[subjectID] = append(bySubject[subjectID], r)
		subjectPaths[subjectID] = subjectPath
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("load edits: %v", err)
		return nil
	}

	result := make(map[int64]*effectiveEdit, len(bySubject))
	for subjectID, candidates := range bySubject {
		// Newest first: the first that is an edit message is the effective one.
		sort.Slice(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.time != b.time {
				return a.time > b.time
			}
			return bytes.Compare(a.hash, b.hash) > 0
		})
		var subject []byte
		subjectRead := false
		for _, r := range candidates {
			eff := &effectiveEdit{msgID: r.msgID, time: r.time, hash: r.hash}
			if r.size == 0 {
				eff.revert = true
				result[subjectID] = eff
				break
			}
			if !subjectRead {
				var ok bool
				if subject, ok = h.readBody(subjectPaths[subjectID]); !ok {
					break
				}
				subjectRead = true
			}
			doc, ok := h.readBody(r.dataPath)
			if !ok {
				continue
			}
			data, err := edit.Apply(doc, subject, h.editLimit(len(subject)))
			if errors.Is(err, edit.ErrMalformed) {
				continue
			}
			if err == nil {
				eff.applied = true
				eff.data = data
			}
			// A well-formed edit too large to apply stays the effective
			// edit: the original data is presented.
			result[subjectID] = eff
			break
		}
	}
	return result
}

// shortTextOf returns the short_text preview of in-memory body data, as
// extractShortText does for a stored body.
func (h *MessageHandler) shortTextOf(data []byte, mimeType string) string {
	if h.ShortTextSize <= 0 || !isTextMIME(mimeType) {
		return ""
	}
	if len(data) > h.ShortTextSize {
		data = data[:h.ShortTextSize]
	}
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}

// populateListEdits fills Edit and Edited on every list item and replaces the
// preview of an edited item with one of its edited data.
func (h *MessageHandler) populateListEdits(ctx context.Context, items []messageListItem) {
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	edits := h.loadEffectiveEdits(ctx, ids)
	for i := range items {
		it := &items[i]
		if editShape(it.HasPid, it.NoReply, it.Terminal, it.Important, it.HasAddTo, it.Type, len(it.Attachments)) {
			_, it.Edit = h.editSubject(ctx, it.ID)
		}
		if eff := edits[it.ID]; eff != nil {
			it.Edited = eff.edited()
			if eff.applied {
				it.ShortText = h.shortTextOf(eff.data, it.Type)
			}
		}
	}
}

// without returns addrs less addr, deduplicated case-insensitively, in order.
func without(addrs []string, addr string) []string {
	seen := map[string]bool{strings.ToLower(addr): true}
	var out []string
	for _, a := range addrs {
		key := strings.ToLower(a)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// editTarget is one edit message to send: the branch it replies to and its
// recipients.
type editTarget struct {
	parentHash *string
	to         []string
}

// editTargets returns the edit messages needed for every reader of subject to
// observe an edit (FMSG-008 Recipients and Add-to Batches): one for the
// original, to its participants other than the editor or to the editor alone
// when there is none, and one for each add-to batch, to the recipients that
// batch added.
func editTargets(subject *models.Message, editor string) []editTarget {
	to := without(subject.To, editor)
	if len(to) == 0 {
		to = []string{editor}
	}
	targets := []editTarget{{parentHash: subject.SHA256, to: to}}
	for _, b := range subject.AddTo {
		if added := without(b.To, editor); len(added) > 0 && b.SHA256 != nil {
			targets = append(targets, editTarget{parentHash: b.SHA256, to: added})
		}
	}
	return targets
}

// editInput is the request body of POST /fmsg/:id/edit.
type editInput struct {
	Data *string `json:"data"` // the complete new body; null reverts to the original
}

// existingEditResponse is the 200 body naming the edit message already in
// force, or nulls when the subject has none.
func existingEditResponse(eff *effectiveEdit) gin.H {
	if eff == nil {
		return gin.H{"id": nil, "time": nil, "sha256": nil}
	}
	return gin.H{"id": eff.msgID, "time": eff.time, "sha256": hex.EncodeToString(eff.hash)}
}

// Edit handles POST /fmsg/:id/edit — edits, or reverts an edit of, the data of
// the caller's own sent message by sending FMSG-008 edit messages. The caller
// supplies the complete new body; the edit document is derived here. An edit
// that leaves the effective data unchanged is idempotent and sends nothing.
func (h *MessageHandler) Edit(c *gin.Context) {
	identity := middleware.GetIdentity(c)
	msgID, ok := resolveID(c, h.q())
	if !ok {
		return
	}

	var in editInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Data != nil && int64(len(*in.Data)) > h.MaxDataSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "message data exceeds maximum size"})
		return
	}

	ctx := c.Request.Context()
	subject, subjectPath, err := h.fetchMessage(ctx, msgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		} else {
			log.Printf("edit %d: fetch: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve message"})
		}
		return
	}
	if draftHidden([]string{identity}, subject.From, subject.Time) || purgedFor(ctx, h.q(), identity, msgID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "message not found"})
		return
	}
	if !sameAddr(subject.From, identity) {
		if !isRecipient(subject.To, identity) && !addToContains(subject.AddTo, identity) {
			c.JSON(http.StatusForbidden, gin.H{"error": "access denied"})
			return
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "only the author may edit a message"})
		return
	}
	if subject.Time == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a draft is changed with PUT, not edited"})
		return
	}
	if subject.Terminal {
		c.JSON(http.StatusConflict, gin.H{"error": "message is terminal; it cannot be edited"})
		return
	}
	if subject.SHA256 == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "message has no finalized identity"})
		return
	}

	original, ok := h.readBody(subjectPath)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "message data not available"})
		return
	}
	current := h.loadEffectiveEdits(ctx, []int64{msgID})[msgID]

	// Idempotency: nothing is sent when the effective data would not change.
	var doc []byte
	if in.Data == nil {
		if current == nil || current.revert {
			c.JSON(http.StatusOK, existingEditResponse(current))
			return
		}
	} else {
		want := []byte(*in.Data)
		unchanged := bytes.Equal(original, want)
		if current != nil && !current.revert {
			unchanged = current.applied && bytes.Equal(current.data, want)
		}
		if unchanged {
			c.JSON(http.StatusOK, existingEditResponse(current))
			return
		}
		doc = edit.Diff(original, want)
	}

	targets := editTargets(subject, identity)

	// As for any reply, refuse when a remote recipient host can never accept
	// it because it does not hold the subject (SPEC §10.3, code 6).
	var all []string
	for _, t := range targets {
		all = append(all, t.to...)
	}
	if replyDomains := remoteRecipientDomains(&models.Message{To: all}, h.LocalDomain); len(replyDomains) > 0 {
		parentFromDomain, byDomain, derr := h.parentDeliveryByDomain(ctx, msgID)
		if derr != nil {
			log.Printf("edit %d: verify delivery: %v", msgID, derr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify parent delivery"})
			return
		}
		if blocked := undeliverableReplyDomains(replyDomains, parentFromDomain, byDomain); len(blocked) > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "edit cannot be accepted by recipient host(s): " + strings.Join(blocked, "; ")})
			return
		}
	}

	// An edit is a message like any other: each one sent counts against the
	// sender's send limits, and its recipients' receive limits.
	size := int64(len(doc))
	if !h.checkSendQuota(c, identity, size*int64(len(targets))) {
		return
	}
	now := float64(time.Now().UnixMicro()) / 1e6
	tx, err := h.q().Begin(ctx)
	if err != nil {
		log.Printf("edit %d: begin: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send edit"})
		return
	}
	defer tx.Rollback(ctx)

	var files message.Files
	onRollback(c, func() { files.Cleanup() })
	editIDs := make([]int64, len(targets))
	var firstHash []byte
	for i, t := range targets {
		var editID int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO msg (version, pid, psha256, no_reply, is_important, is_deflate, is_terminal, from_addr, topic, type, size, filepath, time_sent)
			 VALUES (1, $1, decode($5,'hex'), true, false, false, true, $2, '', $3, $4, '', NULL)
			 RETURNING id`,
			msgID, identity, edit.MediaType, len(doc), t.parentHash,
		).Scan(&editID); err != nil {
			log.Printf("edit %d: insert: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send edit"})
			return
		}
		dataPath, err := h.saveMessageData(identity, editID, ".bin", string(doc))
		if err != nil {
			log.Printf("edit %d: save data: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save edit"})
			return
		}
		createdFile(c, dataPath)
		if _, err := tx.Exec(ctx, "UPDATE msg SET filepath = $1 WHERE id = $2", dataPath, editID); err != nil {
			log.Printf("edit %d: update filepath: %v", msgID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send edit"})
			return
		}
		for _, addr := range t.to {
			if _, err := tx.Exec(ctx, "INSERT INTO msg_to (msg_id, addr) VALUES ($1, $2)", editID, addr); err != nil {
				log.Printf("edit %d: insert recipient %s: %v", msgID, addr, err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send edit"})
				return
			}
		}
		hash, err := message.Finalize(ctx, finalizationTx{tx}, editID, now, &files)
		if err != nil {
			log.Printf("finalize edit: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to finalize edit"})
			return
		}
		editIDs[i] = editID
		if i == 0 {
			firstHash = hash
		}
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("edit %d: commit: %v", msgID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to send edit"})
		return
	}

	// fmsgd's outbound sender skips the local domain, so local recipients
	// have their delivery resolved here, as Send does.
	afterCommit(c, func() {
		plain := *h
		plain.query = nil
		for i, t := range targets {
			plain.recordSend(identity, now, size)
			plain.resolveLocalDelivery(ctx, "msg_to", editIDs[i], h.LocalDomain, t.to)
		}
	})

	c.JSON(http.StatusCreated, gin.H{"id": editIDs[0], "time": now, "sha256": hex.EncodeToString(firstHash)})
}
