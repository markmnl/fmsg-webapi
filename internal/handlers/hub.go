package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event type discriminators for the WebSocket envelope. Adding a new event
// type means adding a constant here and a producer that dispatches it.
const (
	eventNewMsg    = "new_msg"
	eventDelivered = "delivered"
)

// wsEnvelope is the JSON shape of every frame pushed over a WebSocket. The
// Type field lets clients route events; Data carries the event-specific body.
type wsEnvelope struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// Hub maintains the set of connected WebSocket clients and fans out database
// notifications to the clients they pertain to. A single dedicated PostgreSQL
// connection LISTENs on new_msg for the whole process, so the number of
// connected clients does not affect the size of the database connection pool.
type Hub struct {
	// buildItem produces the message payload pushed for a notification. It is
	// a field (rather than a *MessageHandler call) so dispatch can be unit
	// tested without a database.
	buildItem func(ctx context.Context, msgID int64, recipient string) (*messageListItem, error)

	// notifyPush, when non-nil, dispatches a Web Push for a new_msg
	// notification. It runs independently of WebSocket fan-out so pushes are
	// delivered even when the recipient has no live WebSocket.
	notifyPush func(ctx context.Context, msgID int64, addr string)

	mu sync.RWMutex
	// registry maps a lower-cased user address to the set of that user's
	// currently connected clients (a user may have several connections).
	registry map[string]map[*wsClient]struct{}
}

// SetPushNotifier registers a Web Push dispatcher invoked for every new_msg
// notification. It must be called before Run.
func (h *Hub) SetPushNotifier(fn func(ctx context.Context, msgID int64, addr string)) {
	h.notifyPush = fn
}

// NewHub creates a Hub that builds pushed message payloads via msgs.
func NewHub(msgs *MessageHandler) *Hub {
	return &Hub{
		buildItem: msgs.messageItemFor,
		registry:  make(map[string]map[*wsClient]struct{}),
	}
}

// Register adds a client to the registry under its authenticated address.
func (h *Hub) Register(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.registry[c.addr]
	if set == nil {
		set = make(map[*wsClient]struct{})
		h.registry[c.addr] = set
	}
	set[c] = struct{}{}
}

// Unregister removes a client from the registry.
func (h *Hub) Unregister(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.registry[c.addr]
	if set == nil {
		return
	}
	delete(set, c)
	if len(set) == 0 {
		delete(h.registry, c.addr)
	}
}

// Run owns the dedicated listener connection. It blocks until ctx is cancelled,
// reconnecting with capped exponential backoff if the connection drops.
func (h *Hub) Run(ctx context.Context) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for ctx.Err() == nil {
		err := h.listen(ctx, func() { backoff = time.Second })
		if ctx.Err() != nil {
			return
		}
		log.Printf("ws hub: listener stopped (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// listen opens a dedicated connection, LISTENs on new_msg and delivered, and
// dispatches every notification until the connection fails or ctx is
// cancelled. onConnected is invoked once both LISTENs have succeeded so the
// caller can reset its backoff.
func (h *Hub) listen(ctx context.Context, onConnected func()) error {
	// An empty connection string makes pgx read the standard PG* environment
	// variables, exactly as the pgxpool in db.New does.
	conn, err := pgx.Connect(ctx, "")
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN new_msg"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN delivered"); err != nil {
		return err
	}
	log.Println("ws hub: listening on new_msg, delivered")
	onConnected()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		msgID, addr, ok := parseNotifyPayload(n.Payload)
		if !ok {
			log.Printf("ws hub: ignoring malformed notification payload %q", n.Payload)
			continue
		}
		switch n.Channel {
		case "new_msg":
			// Web Push runs in its own goroutine: it is network I/O to many
			// endpoints and must not stall the single listener. It is
			// dispatched here, alongside (not inside) dispatch, so a push is
			// sent even when the recipient has no live WebSocket.
			if h.notifyPush != nil {
				go h.notifyPush(ctx, msgID, addr)
			}
			h.dispatch(ctx, msgID, addr, eventNewMsg)
		case "delivered":
			// addr here is the message's sender (see notify_delivered in
			// dd.sql), not a recipient -- no Web Push for this event yet.
			h.dispatch(ctx, msgID, addr, eventDelivered)
		default:
			log.Printf("ws hub: ignoring notification on unknown channel %q", n.Channel)
		}
	}
}

// parseNotifyPayload parses a new_msg payload of the form "msgID,addr".
func parseNotifyPayload(payload string) (msgID int64, addr string, ok bool) {
	comma := strings.IndexByte(payload, ',')
	if comma < 0 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(payload[:comma], 10, 64)
	if err != nil {
		return 0, "", false
	}
	addr = payload[comma+1:]
	if addr == "" {
		return 0, "", false
	}
	return id, addr, true
}

// dispatch pushes message msgID to every client connected as addr, tagged
// with eventType. The message is fetched and marshalled only when at least
// one such client is connected, so notifications for addresses with no live
// WebSocket cost nothing beyond a map lookup. For eventNewMsg, addr
// originates from a msg_to/msg_add_to row (a recipient); for eventDelivered,
// addr is the message's sender (see notify_delivered in dd.sql). Either way,
// buildItem accepts any participant address and produces the full message
// item, so no special-casing is needed here.
func (h *Hub) dispatch(ctx context.Context, msgID int64, addr string, eventType string) {
	h.mu.RLock()
	set := h.registry[strings.ToLower(addr)]
	clients := make([]*wsClient, 0, len(set))
	for c := range set {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	if len(clients) == 0 {
		return
	}

	item, err := h.buildItem(ctx, msgID, addr)
	if err != nil {
		log.Printf("ws hub: build message %d for %s: %v", msgID, addr, err)
		return
	}
	payload, err := json.Marshal(wsEnvelope{Type: eventType, Data: item})
	if err != nil {
		log.Printf("ws hub: marshal message %d: %v", msgID, err)
		return
	}

	for _, c := range clients {
		select {
		case c.send <- payload:
		default:
			// Slow client: drop the connection rather than stall the
			// shared fan-out for every other client.
			log.Printf("ws hub: client %s send buffer full, closing", c.addr)
			c.close()
		}
	}
}
