package handlers

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	syncatomic "sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// Event type discriminators for the WebSocket envelope. Adding a new event
// type means adding a constant here and a producer that dispatches it.
const (
	eventNewMsg          = "new_msg"
	eventDelivered       = "delivered"
	eventRecipientsAdded = "recipients_added"
	eventReaction        = "reaction"
	eventEdit            = "edit"
	eventRead            = "read"
	eventTrash           = "trash"
	eventIndicator       = "indicator"
)

// wsEnvelope is the JSON shape of every frame pushed over a WebSocket. The
// Type field lets clients route events; Data carries the event-specific body.
type wsEnvelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

const (
	defaultPushWorkers   = 8
	defaultPushQueueSize = 256
	msgClassCacheSize    = 1024
)

type msgClassKind uint8

const (
	msgClassNormal msgClassKind = iota
	msgClassReaction
	msgClassEdit
)

type msgClassification struct {
	kind    msgClassKind
	subject int64
}

// msgClassCache is a bounded FIFO cache of immutable sent-message
// classifications (normal / reaction / edit) so K-recipient fan-out does not
// repeat DB and disk classification queries for every recipient.
type msgClassCache struct {
	mu   sync.Mutex
	m    map[int64]msgClassification
	ring [msgClassCacheSize]int64
	pos  int
	full bool
}

func (c *msgClassCache) get(id int64) (msgClassification, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		return msgClassification{}, false
	}
	v, ok := c.m[id]
	return v, ok
}

func (c *msgClassCache) put(id int64, cls msgClassification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[int64]msgClassification, msgClassCacheSize)
	}
	if _, exists := c.m[id]; exists {
		return
	}
	if c.full {
		delete(c.m, c.ring[c.pos])
	}
	c.ring[c.pos] = id
	c.m[id] = cls
	c.pos++
	if c.pos == len(c.ring) {
		c.pos = 0
		c.full = true
	}
}

type pushJob struct {
	msgID int64
	addr  string
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
	// notification. It runs on a bounded worker pool independently of
	// WebSocket fan-out so pushes are delivered even when the recipient has no
	// live WebSocket.
	notifyPush func(ctx context.Context, msgID int64, addr string)

	// reactionSubject, when non-nil, reports the subject message of msgID
	// when msgID is an FMSG-005 reaction. A reaction is pushed to its
	// recipients as a reaction event carrying the refreshed subject, never
	// as new_msg. Transient errors are returned so the hub does not cache a
	// false negative.
	reactionSubject func(ctx context.Context, msgID int64) (int64, bool, error)
	// editSubject, when non-nil, reports the subject message of msgID when
	// msgID is an FMSG-008 edit. An edit is pushed to its recipients as an
	// edit event carrying the refreshed subject, never as a new message.
	// Transient errors are returned so the hub does not cache a false negative.
	editSubject func(ctx context.Context, msgID int64) (int64, bool, error)

	// indicatorParticipants and threadRoot split indicator audience lookup so
	// routeIndicator can skip the recursive root CTE when no participant has a
	// connected WebSocket. indicatorAudience is kept as a fallback hook for
	// tests.
	indicatorParticipants func(ctx context.Context, msgID int64) ([]string, error)
	threadRoot            func(ctx context.Context, msgID int64) (int64, error)
	indicatorAudience     func(ctx context.Context, msgID int64) ([]string, int64, error)

	classCache msgClassCache

	pushWorkers   int
	pushQueueSize int
	pushMu        sync.Mutex
	pushCh        chan pushJob
	pushCtx       context.Context
	pushCancel    context.CancelFunc
	pushClosed    bool
	pushWG        sync.WaitGroup
	pushDrops     syncatomic.Uint64

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

// PushDrops returns the total number of Web Push jobs dropped because the
// bounded push queue was full.
func (h *Hub) PushDrops() uint64 {
	return h.pushDrops.Load()
}

// Close shuts down the bounded Web Push worker pool, cancelling queued and
// in-flight pushes and waiting until all workers have exited.
func (h *Hub) Close() {
	_ = h.CloseContext(context.Background())
}

// CloseContext shuts down the bounded Web Push worker pool, cancelling its
// push context so queued jobs are skipped and in-flight pushes abort promptly,
// and waits for workers to exit until ctx is cancelled.
func (h *Hub) CloseContext(ctx context.Context) error {
	h.pushMu.Lock()
	if !h.pushClosed {
		h.pushClosed = true
		if h.pushCancel != nil {
			h.pushCancel()
		}
		if h.pushCh != nil {
			close(h.pushCh)
		}
	}
	h.pushMu.Unlock()

	if ctx == nil {
		h.pushWG.Wait()
		return nil
	}
	done := make(chan struct{})
	go func() {
		h.pushWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hub) enqueuePush(msgID int64, addr string) {
	h.pushMu.Lock()
	if h.pushClosed {
		h.pushMu.Unlock()
		return
	}
	if h.pushCh == nil {
		workers := h.pushWorkers
		if workers <= 0 {
			workers = defaultPushWorkers
		}
		qsize := h.pushQueueSize
		if qsize <= 0 {
			qsize = defaultPushQueueSize
		}
		h.pushCtx, h.pushCancel = context.WithCancel(context.Background())
		h.pushCh = make(chan pushJob, qsize)
		h.pushWG.Add(workers)
		for range workers {
			go h.pushWorker(h.pushCtx)
		}
	}
	select {
	case h.pushCh <- pushJob{msgID: msgID, addr: addr}:
	default:
		drops := h.pushDrops.Add(1)
		log.Printf("ws hub: push queue full, dropping push for msg %d to %s (total dropped: %d)", msgID, addr, drops)
	}
	h.pushMu.Unlock()
}

func (h *Hub) pushWorker(pushCtx context.Context) {
	defer h.pushWG.Done()
	for job := range h.pushCh {
		if pushCtx.Err() != nil {
			return
		}
		h.notifyPush(pushCtx, job.msgID, job.addr)
	}
}

// NewHub creates a Hub that builds pushed message payloads via msgs.
func NewHub(msgs *MessageHandler) *Hub {
	return &Hub{
		buildItem:             msgs.messageItemFor,
		reactionSubject:       msgs.reactionSubject,
		editSubject:           msgs.editSubject,
		indicatorParticipants: msgs.indicatorParticipants,
		threadRoot:            msgs.threadRoot,
		indicatorAudience:     msgs.indicatorAudience,
		registry:              make(map[string]map[*wsClient]struct{}),
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

// listen opens a dedicated connection, LISTENs on every notification channel,
// and routes each notification until the connection fails or ctx is
// cancelled. onConnected is invoked once every LISTEN has succeeded so the
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
	if _, err := conn.Exec(ctx, "LISTEN recipients_added"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN msg_read"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN msg_trash"); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "LISTEN "+IndicatorChannel); err != nil {
		return err
	}
	log.Println("ws hub: listening on new_msg, delivered, recipients_added, msg_read, msg_trash, indicator")
	onConnected()

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if n.Channel == IndicatorChannel {
			h.routeIndicator(ctx, n.Payload)
			continue
		}
		msgID, addr, ok := parseNotifyPayload(n.Payload)
		if !ok {
			log.Printf("ws hub: ignoring malformed notification payload %q", n.Payload)
			continue
		}
		h.route(ctx, n.Channel, msgID, addr)
	}
}

// classifyNewMsg classifies an immutable sent message once and caches the
// result so multi-recipient fan-out avoids repeated DB/disk queries. Transient
// classification errors are not cached so subsequent recipients retry.
func (h *Hub) classifyNewMsg(ctx context.Context, msgID int64) msgClassification {
	if h.reactionSubject == nil && h.editSubject == nil {
		return msgClassification{kind: msgClassNormal}
	}
	if cls, ok := h.classCache.get(msgID); ok {
		return cls
	}
	cls := msgClassification{kind: msgClassNormal}
	cacheable := true
	if h.reactionSubject != nil {
		subject, ok, err := h.reactionSubject(ctx, msgID)
		if err != nil {
			cacheable = false
		} else if ok {
			cls = msgClassification{kind: msgClassReaction, subject: subject}
		}
	}
	if cls.kind == msgClassNormal && h.editSubject != nil {
		subject, ok, err := h.editSubject(ctx, msgID)
		if err != nil {
			cacheable = false
		} else if ok {
			cls = msgClassification{kind: msgClassEdit, subject: subject}
		}
	}
	if cacheable {
		h.classCache.put(msgID, cls)
	}
	return cls
}

// route handles one notification on channel for message msgID and address
// addr.
func (h *Hub) route(ctx context.Context, channel string, msgID int64, addr string) {
	switch channel {
	case "new_msg":
		if h.notifyPush == nil && !h.hasClients(addr) {
			return
		}
		cls := h.classifyNewMsg(ctx, msgID)
		switch cls.kind {
		case msgClassReaction:
			// FMSG-005: a reaction is a low-priority event on its
			// subject, not a new message. No Web Push is sent.
			h.dispatch(ctx, cls.subject, addr, eventReaction)
			return
		case msgClassEdit:
			// FMSG-008: an edit is a low-priority event on its
			// subject, not a new message. No Web Push is sent.
			h.dispatch(ctx, cls.subject, addr, eventEdit)
			return
		}
		// Web Push runs on a bounded worker pool: it is network I/O to many
		// endpoints and must not stall the single listener. It is
		// dispatched here, alongside (not inside) dispatch, so a push is
		// sent even when the recipient has no live WebSocket.
		if h.notifyPush != nil {
			h.enqueuePush(msgID, addr)
		}
		h.dispatch(ctx, msgID, addr, eventNewMsg)
	case "delivered":
		// addr here is the message's sender (see notify_delivered in
		// dd.sql), not a recipient -- no Web Push for this event yet.
		h.dispatch(ctx, msgID, addr, eventDelivered)
	case "msg_read":
		// A recipient marked the message read (see MarkRead); addr is that
		// recipient. Pushes the message with its read state so the reader's
		// other tabs and devices stop showing it as unread.
		h.dispatch(ctx, msgID, addr, eventRead)
	case "msg_trash":
		// The address moved the message into or out of its trash, or
		// deleted it for good (see trash.go). Its other tabs and devices
		// get the message with its trash state.
		h.dispatchTrash(ctx, msgID, addr)
	case "recipients_added":
		// An add-to batch was recorded against the message; addr is one
		// of its participants (see notify_recipients_added in fmsgd's
		// dd.sql). Pushes the refreshed message so clients can show the
		// updated recipient list.
		h.dispatch(ctx, msgID, addr, eventRecipientsAdded)
	default:
		log.Printf("ws hub: ignoring notification on unknown channel %q", channel)
	}
}

// parseNotifyPayload parses a new_msg payload of the form "msgID,addr".
func parseNotifyPayload(payload string) (msgID int64, addr string, ok bool) {
	before, after, ok := strings.Cut(payload, ",")
	if !ok {
		return 0, "", false
	}
	id, err := strconv.ParseInt(before, 10, 64)
	if err != nil {
		return 0, "", false
	}
	addr = after
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
	if item.purged {
		return // deleted for good by addr: never shown to it again
	}
	h.send(clients, msgID, wsEnvelope{Type: eventType, Data: item})
}

// trashGone is the trash event data for a message deleted for good.
type trashGone struct {
	ID    int64  `json:"id"`
	Trash string `json:"trash"`
}

// dispatchTrash pushes a trash event to addr's clients: the message with its
// trash state, or only its id when addr deleted it for good (it may no longer
// exist).
func (h *Hub) dispatchTrash(ctx context.Context, msgID int64, addr string) {
	clients := h.clientsOf(addr)
	if len(clients) == 0 {
		return
	}
	item, err := h.buildItem(ctx, msgID, addr)
	if err != nil || item.purged {
		h.send(clients, msgID, wsEnvelope{Type: eventTrash, Data: trashGone{ID: msgID, Trash: trashPurged}})
		return
	}
	h.send(clients, msgID, wsEnvelope{Type: eventTrash, Data: item})
}

// hasClients reports whether at least one client is connected as addr.
func (h *Hub) hasClients(addr string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.registry[strings.ToLower(addr)]) > 0
}

// hasAnyClients reports whether any WebSocket client is currently connected.
func (h *Hub) hasAnyClients() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.registry) > 0
}

// clientsOf lists the clients connected as addr.
func (h *Hub) clientsOf(addr string) []*wsClient {
	h.mu.RLock()
	defer h.mu.RUnlock()
	set := h.registry[strings.ToLower(addr)]
	clients := make([]*wsClient, 0, len(set))
	for c := range set {
		clients = append(clients, c)
	}
	return clients
}

// clientsOfParticipants collects connected clients for every unique address in
// addrs except exclude, under a single RLock acquisition.
func (h *Hub) clientsOfParticipants(addrs []string, exclude string) []*wsClient {
	seen := map[string]bool{strings.ToLower(exclude): true}
	h.mu.RLock()
	defer h.mu.RUnlock()
	var clients []*wsClient
	for _, a := range addrs {
		k := strings.ToLower(a)
		if seen[k] {
			continue
		}
		seen[k] = true
		for c := range h.registry[k] {
			clients = append(clients, c)
		}
	}
	return clients
}

// send marshals env and queues it on each client.
func (h *Hub) send(clients []*wsClient, msgID int64, env wsEnvelope) {
	payload, err := json.Marshal(env)
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

// routeIndicator delivers an FMSG-006 indicator (a JSON Indicator payload on
// the indicator channel) to every connected participant of its message except
// the indicating one. It is sent as is, with the thread's first message id
// added; nothing is stored and no Web Push is sent.
func (h *Hub) routeIndicator(ctx context.Context, payload string) {
	var ind Indicator
	if err := json.Unmarshal([]byte(payload), &ind); err != nil || ind.ID <= 0 || ind.From == "" {
		log.Printf("ws hub: ignoring malformed indicator payload %q", payload)
		return
	}
	if !h.hasAnyClients() {
		return
	}
	var clients []*wsClient
	if h.indicatorParticipants != nil && h.threadRoot != nil {
		addrs, err := h.indicatorParticipants(ctx, ind.ID)
		if err != nil {
			log.Printf("ws hub: indicator on %d: %v", ind.ID, err)
			return
		}
		clients = h.clientsOfParticipants(addrs, ind.From)
		if len(clients) == 0 {
			return
		}
		root, err := h.threadRoot(ctx, ind.ID)
		if err != nil {
			log.Printf("ws hub: indicator root on %d: %v", ind.ID, err)
			return
		}
		ind.RootID = root
	} else if h.indicatorAudience != nil {
		addrs, root, err := h.indicatorAudience(ctx, ind.ID)
		if err != nil {
			log.Printf("ws hub: indicator on %d: %v", ind.ID, err)
			return
		}
		clients = h.clientsOfParticipants(addrs, ind.From)
		if len(clients) == 0 {
			return
		}
		ind.RootID = root
	} else {
		return
	}
	h.send(clients, ind.ID, wsEnvelope{Type: eventIndicator, Data: ind})
}
