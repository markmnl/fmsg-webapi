package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"github.com/markmnl/fmsg-webapi/db"
	"github.com/markmnl/fmsg-webapi/middleware"
)

// pushTimeout caps how long a single new_msg push fan-out may take. It runs on
// a fresh context so a listener reconnect cannot cancel in-flight sends.
const pushTimeout = 30 * time.Second

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

	// send delivers one encrypted push and returns the HTTP status of the
	// push service. It is a field so tests can substitute a fake transport.
	send func(ctx context.Context, payload []byte, sub *webpush.Subscription) (int, error)
}

// NewPushHandler creates a PushHandler. vapidPublic/vapidPrivate are URL-safe
// base64 VAPID keys; vapidSubject is the VAPID "sub" (e.g. "mailto:admin@example.com").
func NewPushHandler(database *db.DB, msgs *MessageHandler, vapidPublic, vapidPrivate, vapidSubject, iconURL string) *PushHandler {
	h := &PushHandler{
		DB:           database,
		msgs:         msgs,
		vapidPublic:  vapidPublic,
		vapidPrivate: vapidPrivate,
		vapidSubject: vapidSubject,
		iconURL:      iconURL,
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
func (h *PushHandler) NotifyNewMsg(_ context.Context, msgID int64, addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
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
		if scanErr := rows.Scan(&s.endpoint, &s.p256dh, &s.auth); scanErr == nil {
			subs = append(subs, s)
		}
	}
	rows.Close()
	if len(subs) == 0 {
		return
	}

	rootID, err := h.rootMsgID(ctx, msgID)
	if err != nil {
		log.Printf("push notify: resolve root of message %d: %v", msgID, err)
		return
	}

	item, err := h.msgs.messageItemFor(ctx, msgID, addr)
	if err != nil {
		log.Printf("push notify: build message %d for %s: %v", msgID, addr, err)
		return
	}

	payload, err := json.Marshal(buildPushPayload(item, rootID, h.iconURL))
	if err != nil {
		log.Printf("push notify: marshal message %d: %v", msgID, err)
		return
	}

	for _, s := range subs {
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
	resp, err := webpush.SendNotificationWithContext(ctx, payload, sub, &webpush.Options{
		Subscriber:      h.vapidSubject,
		VAPIDPublicKey:  h.vapidPublic,
		VAPIDPrivateKey: h.vapidPrivate,
		TTL:             pushTTL,
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
