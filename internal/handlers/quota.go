package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// Quotas. fmsgid holds each address's limits and usage; this service checks
// and records the messages that don't pass through fmsgd's receive path:
//
//   - Sending (send and react): the sender's send limits are checked before
//     the message is sent, and the message is recorded once with POST
//     /fmsgid/send after it is sent, whatever its number of recipients.
//   - Local delivery (see resolveLocalDelivery): each local recipient's
//     receive limits are checked as fmsgd checks them, a recipient over a
//     limit is recorded with response code 101 (user full) and not delivered,
//     and each delivered recipient is recorded with POST /fmsgid/recv.
//
// Every size is the stored size: the message body plus its attachments.
// Usage is timestamped when this service sent or delivered the message.
// Recording failures are logged and never fail the request, because the
// message has already been sent.

// Per-recipient response codes (fmsg specification) that local delivery
// records in msg_to/msg_add_to.response_code, as fmsgd does for recipients of
// the messages it receives.
const (
	responseCodeUserUnknown      = 100
	responseCodeUserFull         = 101
	responseCodeUserNotAccepting = 102
	responseCodeAccept           = 200
)

// recvLimitExceeded reports whether one more message of size bytes would take
// an address over any of its receive limits (-1 is unlimited). These are the
// checks fmsgd makes for a recipient before accepting a message.
func recvLimitExceeded(d *middleware.FmsgIDDetail, size int64) bool {
	return (d.LimitRecvCountPer1d > -1 && d.RecvCountPer1d+1 > d.LimitRecvCountPer1d) ||
		(d.LimitRecvSizePer1d > -1 && d.RecvSizePer1d+size > d.LimitRecvSizePer1d) ||
		(d.LimitRecvSizeTotal > -1 && d.RecvSizeTotal+size > d.LimitRecvSizeTotal)
}

// localDeliveryCode is the response code local delivery records for a
// recipient, given fmsgid's status and detail for it and the message's stored
// size. ok is false for an unexpected fmsgid status, which leaves the
// recipient pending.
func localDeliveryCode(status int, d *middleware.FmsgIDDetail, size int64) (code int, ok bool) {
	switch {
	case status == http.StatusNotFound:
		return responseCodeUserUnknown, true
	case status != http.StatusOK || d == nil:
		return 0, false
	case !d.AcceptingNew:
		return responseCodeUserNotAccepting, true
	case recvLimitExceeded(d, size):
		return responseCodeUserFull, true
	default:
		return responseCodeAccept, true
	}
}

// sendLimitError is the refusal for a message over one of its sender's send
// limits.
type sendLimitError struct {
	status  int
	code    string
	message string
}

// checkSendLimits checks one more message of size bytes against a sender's
// send limits (-1 is unlimited), and returns the refusal for the first limit
// it would exceed, or nil. A message too big to ever send is 413, a daily
// limit is 429 (it frees up as the day's messages age out), and the sent-mail
// storage limit is 422.
func checkSendLimits(d *middleware.FmsgIDDetail, size int64) *sendLimitError {
	switch {
	case d.LimitSendSizePerMsg > -1 && size > d.LimitSendSizePerMsg:
		return &sendLimitError{http.StatusRequestEntityTooLarge, "send_size_per_msg_limit",
			fmt.Sprintf("message size %d exceeds the sending limit of %d bytes per message", size, d.LimitSendSizePerMsg)}
	case d.LimitSendCountPer1d > -1 && d.SendCountPer1d+1 > d.LimitSendCountPer1d:
		return &sendLimitError{http.StatusTooManyRequests, "send_count_per_1d_limit", "daily message limit reached"}
	case d.LimitSendSizePer1d > -1 && d.SendSizePer1d+size > d.LimitSendSizePer1d:
		return &sendLimitError{http.StatusTooManyRequests, "send_size_per_1d_limit", "daily sending size limit reached"}
	case d.LimitSendSizeTotal > -1 && d.SendSizeTotal+size > d.LimitSendSizeTotal:
		return &sendLimitError{http.StatusUnprocessableEntity, "send_size_total_limit", "storage limit reached"}
	}
	return nil
}

// checkSendQuota checks that sender may send one more message of size bytes,
// with fmsgid's current usage (never the cached lookup). Otherwise it answers
// with the refusal and returns false.
func (h *MessageHandler) checkSendQuota(c *gin.Context, sender string, size int64) bool {
	detail, status, err := middleware.FetchFmsgIDDetail(h.IDURL, sender)
	if err != nil {
		log.Printf("send quota: fmsgid detail %s: %v", sender, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return false
	}
	switch {
	case status == http.StatusNotFound:
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("User %s not found", sender)})
		return false
	case status != http.StatusOK:
		log.Printf("send quota: unexpected fmsgid status %d for %s", status, sender)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "identity service unavailable"})
		return false
	case !detail.AcceptingNew:
		c.JSON(http.StatusForbidden, gin.H{"error": fmt.Sprintf("User %s not authorised to send new messages", sender)})
		return false
	}
	if refusal := checkSendLimits(detail, size); refusal != nil {
		log.Printf("send quota: %s refused: %s", sender, refusal.code)
		c.JSON(refusal.status, gin.H{"error": refusal.message, "code": refusal.code})
		return false
	}
	return true
}

// recordSend records one sent message of size bytes for sender at ts. A
// failure is logged: the message has been sent.
func (h *MessageHandler) recordSend(sender string, ts float64, size int64) {
	if err := middleware.RecordFmsgIDSend(h.IDURL, sender, ts, size); err != nil {
		log.Printf("record send usage: %s: %v", sender, err)
	}
}

// messageStoredSize is a message's stored size: its body plus its
// attachments.
func (h *MessageHandler) messageStoredSize(ctx context.Context, msgID int64) (int64, error) {
	var size int64
	err := h.q().QueryRow(ctx,
		`SELECT m.size + COALESCE((SELECT sum(a.filesize) FROM msg_attachment a WHERE a.msg_id = m.id), 0)
		 FROM msg m WHERE m.id = $1`, msgID).Scan(&size)
	return size, err
}
