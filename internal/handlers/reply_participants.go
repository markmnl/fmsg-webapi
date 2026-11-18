package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/internal/models"
)

// A reply is accepted only by hosts that hold its parent (SPEC §10.3, reject
// code 6). Every recipient of a reply must therefore already have the parent
// it names: a participant of that message. The participants of a message are
// its sender and recipients; those of an add-to batch are also the batch's
// adder and the recipients it added (a batch goes to every participant
// domain, SPEC "Notes on Adding Recipients"). Someone only added in a batch
// has the batch, not the original, so a reply that includes them must name
// the batch by its sha256; anyone new is brought in with add-to.
//
// That is the rule for the reply's to. Recipients added to the reply itself
// (add-to, on the draft or once sent) are outside it: they get the reply as
// an add-to message, whose pid names the reply rather than the reply's
// parent, and a host that doesn't hold a message someone is added to takes
// it as a full delivery (SPEC §10.3 step 7).

// replyParticipants returns the lower-cased participants of the parent a
// reply names: parentID, or the add-to batch of it whose hash is psha256.
func (h *MessageHandler) replyParticipants(ctx context.Context, parentID int64, psha256 *string) (map[string]bool, error) {
	out := map[string]bool{}
	var from string
	if err := h.q().QueryRow(ctx, `SELECT from_addr FROM msg WHERE id = $1`, parentID).Scan(&from); err != nil {
		return nil, err
	}
	out[strings.ToLower(from)] = true
	rows, err := h.q().Query(ctx, `SELECT addr FROM msg_to WHERE msg_id = $1`, parentID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		out[strings.ToLower(a)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if psha256 == nil || *psha256 == "" {
		return out, nil
	}
	var batchID int64
	var adder string
	err = h.q().QueryRow(ctx, `SELECT id, add_to_from FROM msg_add_to_batch WHERE msg_id = $1 AND sha256 = decode($2, 'hex')`, parentID, *psha256).Scan(&batchID, &adder)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil // the parent named by its own hash
	}
	if err != nil {
		return nil, err
	}
	out[strings.ToLower(adder)] = true
	rows, err = h.q().Query(ctx, `SELECT addr FROM msg_add_to WHERE msg_id = $1 AND batch_id = $2`, parentID, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[strings.ToLower(a)] = true
	}
	return out, rows.Err()
}

// checkReplyRecipients answers 409 and reports false when a reply has a
// recipient who isn't a participant of the parent it names. Send is the gate;
// create and update check too, so that a draft that could never be sent
// isn't written.
func (h *MessageHandler) checkReplyRecipients(c *gin.Context, msg *models.Message) bool {
	if msg.PID == nil {
		return true
	}
	parts, err := h.replyParticipants(c.Request.Context(), *msg.PID, msg.PSHA256)
	if err != nil {
		log.Printf("verify reply recipients against parent %d: %v", *msg.PID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to verify reply recipients"})
		return false
	}
	var missing []string
	seen := map[string]bool{}
	for _, a := range msg.To {
		k := strings.ToLower(a)
		if !parts[k] && !seen[k] {
			seen[k] = true
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return true
	}
	c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf(
		"a reply's recipients must all have the message it replies to: %s %s not a participant of it — "+
			"reply to their add-to batch (by its sha256) if they were added, or add them to the message (add-to)",
		strings.Join(missing, ", "), map[bool]string{true: "is", false: "are"}[len(missing) == 1])})
	return false
}
