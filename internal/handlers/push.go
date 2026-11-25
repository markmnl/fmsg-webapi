package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/internal/db"
	"github.com/markmnl/fmsg-webapi/internal/middleware"
)

// pushTimeout caps how long a single new_msg push fan-out may take. It derives
// from the hub's push context so listener reconnects cannot cancel in-flight
// sends while graceful shutdown aborts them promptly.
const pushTimeout = 30 * time.Second

// pushRequestTimeout caps each individual HTTP request to a Web Push endpoint.
const pushRequestTimeout = 10 * time.Second

// pushTTL is the seconds a push service should retain an undelivered message.
const pushTTL = 86400

// PushHandler stores Web Push subscriptions and delivers encrypted pushes via
// VAPID whenever a new_msg notification fires for a recipient.
type PushHandler struct {
	DB           *db.DB
	msgs         *MessageHandler
	vapidPublic  string
	vapidPrivate string
	vapidSubject string
	iconURL      string
	httpClient   *http.Client

	// send delivers one encrypted push and returns the HTTP status of the
	// push service. It is a field so tests can substitute a fake transport.
	send func(ctx context.Context, payload []byte, sub *webpush.Subscription) (int, error)
}

// NewPushHandler creates a PushHandler. vapidPublic/vapidPrivate are URL-safe
// base64 VAPID keys; vapidSubject is the VAPID "sub" (e.g. "mailto:admin@example.com").
func NewPushHandler(database *db.DB, msgs *MessageHandler, vapidPublic, vapidPrivate, vapidSubject, iconURL string) *PushHandler {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16
	h := &PushHandler{
		DB:           database,
		msgs:         msgs,
		vapidPublic:  vapidPublic,
		vapidPrivate: vapidPrivate,
		vapidSubject: vapidSubject,
		iconURL:      iconURL,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   pushRequestTimeout,
		},
	}
	h.send = h.sendNotification
	return h
}

// pushSubscriptionInput is the browser PushSubscription.toJSON() body. The
// expirationTime field is accepted but ignored.
type pushSubscriptionInput struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushUnsubscribeInput is the DELETE body identifying a subscription to remove.
type pushUnsubscribeInput struct {
	Endpoint string `json:"endpoint"`
}

// pushPayload is the JSON body delivered to the client service worker, which
// decides whether to display a notification.
type pushPayload struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	ThreadID int64  `json:"threadId"`
	URL      string `json:"url"`
	Tag      string `json:"tag"`
	Icon     string `json:"icon"`
}

// Subscribe handles POST /fmsg/push/subscribe — upserts the authenticated
// user's push subscription, keyed by (addr, endpoint).
func (h *PushHandler) Subscribe(c *gin.Context) {
	identity := middleware.GetIdentity(c)

	var in pushSubscriptionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Endpoint == "" || in.Keys.P256dh == "" || in.Keys.Auth == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint, keys.p256dh and keys.auth are required"})
		return
	}

	if _, err := h.DB.Pool.Exec(c.Request.Context(),
		`INSERT INTO push_subscription (addr, endpoint, p256dh, auth)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (addr, endpoint) DO UPDATE SET p256dh = $3, auth = $4`,
		identity, in.Endpoint, in.Keys.P256dh, in.Keys.Auth,
	); err != nil {
		log.Printf("push subscribe: addr=%s: %v", identity, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store subscription"})
		return
	}

	c.Status(http.StatusCreated)
}

// Unsubscribe handles DELETE /fmsg/push/subscribe — removes the named
// subscription for the authenticated user. It is idempotent.
func (h *PushHandler) Unsubscribe(c *gin.Context) {
	identity := middleware.GetIdentity(c)

	var in pushUnsubscribeInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if in.Endpoint == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "endpoint is required"})
		return
	}

	if _, err := h.DB.Pool.Exec(c.Request.Context(),
		"DELETE FROM push_subscription WHERE addr = $1 AND endpoint = $2",
		identity, in.Endpoint,
	); err != nil {
		log.Printf("push unsubscribe: addr=%s: %v", identity, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove subscription"})
		return
	}

	c.Status(http.StatusNoContent)
}

// NotifyNewMsg delivers a Web Push for message msgID to every subscription of
// recipient addr. It is invoked by the WebSocket hub for each new_msg
// notification and runs independently of WebSocket fan-out, so a recipient with
// no live WebSocket is still notified.
func (h *PushHandler) NotifyNewMsg(ctx context.Context, msgID int64, addr string) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()

	// Load the recipient's subscriptions first; an address with none costs
	// only a single indexed query.
	type subRow struct {
		endpoint, p256dh, auth string
	}
	rows, err := h.DB.Pool.Query(ctx,
		"SELECT endpoint, p256dh, auth FROM push_subscription WHERE addr = $1", addr)
	if err != nil {
		log.Printf("push notify: load subscriptions for %s: %v", addr, err)
		return
	}
	var subs []subRow
	for rows.Next() {
		var s subRow
		if scanErr := rows.Scan(&s.endpoint, &s.p256dh, &s.auth); scanErr != nil {
			rows.Close()
			log.Printf("push notify: scan subscriptions for %s: %v", addr, scanErr)
			return
		}
		subs = append(subs, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		log.Printf("push notify: iterate subscriptions for %s: %v", addr, err)
		return
	}
	if len(subs) == 0 {
		return
	}

	// Load only the message fields needed for push filtering and payload
	// construction rather than building a full message list item.
	var fromAddr, mediaType, dataPath string
	var pid *int64
	var noReply, terminal, important, hasAttachments, hasAddTo bool
	var size int
	var trashState *string
	if err := h.DB.Pool.QueryRow(ctx, `
		SELECT m.from_addr, m.pid, m.no_reply, m.is_terminal, m.is_important,
		       m.type, m.size, m.filepath,
		       EXISTS (SELECT 1 FROM msg_attachment a WHERE a.msg_id = m.id),
		       EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id = m.id),
		       CASE WHEN EXISTS (SELECT 1 FROM fmsg_api_trash WHERE addr = $2)
		            THEN fmsg_api_trash_state($2, m.id) END
		FROM msg m
		WHERE m.id = $1`, msgID, strings.ToLower(addr),
	).Scan(&fromAddr, &pid, &noReply, &terminal, &important, &mediaType, &size, &dataPath, &hasAttachments, &hasAddTo, &trashState); err != nil {
		log.Printf("push notify: load message %d for %s: %v", msgID, addr, err)
		return
	}
	if trashState != nil {
		return // in the recipient's trash or purged: they aren't told
	}
	attCount := 0
	if hasAttachments {
		attCount = 1
	}
	if h.msgs.reactionOf(pid != nil, noReply, terminal, important, hasAddTo, mediaType, size, attCount, dataPath) != nil {
		return // FMSG-005: a reaction does not warrant a notification
	}
	if editShape(pid != nil, noReply, terminal, important, hasAddTo, mediaType, attCount) {
		if _, isEdit, _ := h.msgs.editSubject(ctx, msgID); isEdit {
			return // FMSG-008: nor does an edit
		}
	}

	rootID, err := h.rootMsgID(ctx, msgID)
	if err != nil {
		log.Printf("push notify: resolve root of message %d: %v", msgID, err)
		return
	}

	item := &messageListItem{
		From:      fromAddr,
		ShortText: h.msgs.extractShortText(dataPath, mediaType),
	}
	payload, err := json.Marshal(buildPushPayload(item, rootID, h.iconURL))
	if err != nil {
		log.Printf("push notify: marshal message %d: %v", msgID, err)
		return
	}

	for _, s := range subs {
		if ctx.Err() != nil {
			return
		}
		sub := &webpush.Subscription{
			Endpoint: s.endpoint,
			Keys:     webpush.Keys{P256dh: s.p256dh, Auth: s.auth},
		}
		status, err := h.send(ctx, payload, sub)
		if err != nil {
			log.Printf("push notify: send to %s for %s: %v", s.endpoint, addr, err)
			continue
		}
		if shouldPrune(status) {
			if _, delErr := h.DB.Pool.Exec(ctx,
				"DELETE FROM push_subscription WHERE addr = $1 AND endpoint = $2",
				addr, s.endpoint,
			); delErr != nil {
				log.Printf("push notify: prune %s for %s: %v", s.endpoint, addr, delErr)
			}
			continue
		}
		if status < 200 || status >= 300 {
			log.Printf("push notify: send to %s for %s: status %d", s.endpoint, addr, status)
		}
	}
}

// buildPushPayload assembles the service-worker payload for a message. threadId
// and the deep-link URL reference the thread's root message so replies group
// with their thread.
func buildPushPayload(item *messageListItem, rootID int64, iconURL string) pushPayload {
	return pushPayload{
		Title:    item.From,
		Body:     item.ShortText,
		ThreadID: rootID,
		URL:      fmt.Sprintf("/app3.html?thread=%d", rootID),
		Tag:      fmt.Sprintf("thread-%d", rootID),
		Icon:     iconURL,
	}
}

// shouldPrune reports whether a push-service HTTP status means the subscription
// is gone and should be deleted (404 Not Found, 410 Gone).
func shouldPrune(status int) bool {
	return status == http.StatusNotFound || status == http.StatusGone
}

// rootMsgID walks the msg.pid chain up to the thread root (the message whose
// pid is NULL) and returns its id.
func (h *PushHandler) rootMsgID(ctx context.Context, msgID int64) (int64, error) {
	var rootID int64
	err := h.DB.Pool.QueryRow(ctx,
		`WITH RECURSIVE chain AS (
		     SELECT id, pid FROM msg WHERE id = $1
		     UNION ALL
		     SELECT m.id, m.pid FROM msg m JOIN chain c ON m.id = c.pid
		 )
		 SELECT id FROM chain WHERE pid IS NULL`,
		msgID,
	).Scan(&rootID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Defensive: a message with no NULL-pid ancestor (should not happen)
		// — treat the message itself as the thread root.
		return msgID, nil
	}
	if err != nil {
		return 0, err
	}
	return rootID, nil
}

// sendNotification is the production send implementation: it encrypts and POSTs
// one push via VAPID and returns the push service's HTTP status code.
func (h *PushHandler) sendNotification(ctx context.Context, payload []byte, sub *webpush.Subscription) (int, error) {
	opts := &webpush.Options{
		Subscriber:      h.vapidSubject,
		VAPIDPublicKey:  h.vapidPublic,
		VAPIDPrivateKey: h.vapidPrivate,
		TTL:             pushTTL,
		HTTPClient:      h.httpClient,
	}
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, opts)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}
