package handlers

import (
	"context"
	"encoding/json"
	"errors"
	syncatomic "sync/atomic"
	"testing"
	"time"
)

func newTestHub() *Hub {
	return &Hub{
		buildItem: func(_ context.Context, msgID int64, recipient string) (*messageListItem, error) {
			return &messageListItem{ID: msgID, From: "@sender@x", To: []string{recipient}}, nil
		},
		registry: make(map[string]map[*wsClient]struct{}),
	}
}

func newTestClient(addr string) *wsClient {
	return &wsClient{
		addr: addr,
		send: make(chan []byte, wsSendBuffer),
		done: make(chan struct{}),
	}
}

func TestHubDispatch_RoutesOnlyToParticipant(t *testing.T) {
	hub := newTestHub()
	alice := newTestClient("@alice@x")
	bob := newTestClient("@bob@x")
	hub.Register(alice)
	hub.Register(bob)

	// Mixed-case payload address must still route to the lower-cased registry.
	hub.dispatch(context.Background(), 42, "@Alice@x", eventNewMsg)

	select {
	case payload := <-alice.send:
		var env wsEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != eventNewMsg {
			t.Fatalf("envelope type = %q, want %q", env.Type, eventNewMsg)
		}
	default:
		t.Fatal("alice should have received the dispatched message")
	}

	select {
	case <-bob.send:
		t.Fatal("bob must not receive a message addressed to alice")
	default:
	}
}

func TestHubDispatch_NoClientsSkipsBuild(t *testing.T) {
	built := false
	hub := &Hub{
		buildItem: func(_ context.Context, msgID int64, _ string) (*messageListItem, error) {
			built = true
			return &messageListItem{ID: msgID}, nil
		},
		registry: make(map[string]map[*wsClient]struct{}),
	}
	hub.dispatch(context.Background(), 1, "@nobody@x", eventNewMsg)
	if built {
		t.Fatal("buildItem must not be called when no client is connected")
	}
}

func TestHubDispatch_UsesGivenEventType(t *testing.T) {
	hub := newTestHub()
	alice := newTestClient("@alice@x")
	hub.Register(alice)

	hub.dispatch(context.Background(), 42, "@alice@x", eventDelivered)

	select {
	case payload := <-alice.send:
		var env wsEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != eventDelivered {
			t.Fatalf("envelope type = %q, want %q", env.Type, eventDelivered)
		}
	default:
		t.Fatal("alice should have received the dispatched message")
	}
}

func TestHubRegisterUnregister(t *testing.T) {
	hub := newTestHub()
	c1 := newTestClient("@u@x")
	c2 := newTestClient("@u@x")
	hub.Register(c1)
	hub.Register(c2)
	if got := len(hub.registry["@u@x"]); got != 2 {
		t.Fatalf("registry size = %d, want 2", got)
	}
	hub.Unregister(c1)
	hub.Unregister(c2)
	if _, ok := hub.registry["@u@x"]; ok {
		t.Fatal("empty address entry should be removed from the registry")
	}
}

func TestParseNotifyPayload(t *testing.T) {
	tests := []struct {
		payload string
		id      int64
		addr    string
		ok      bool
	}{
		{"42,@alice@x", 42, "@alice@x", true},
		{"notanumber,@a@b", 0, "", false},
		{"42", 0, "", false},
		{"42,", 0, "", false},
		{"", 0, "", false},
	}
	for _, tt := range tests {
		id, addr, ok := parseNotifyPayload(tt.payload)
		if ok != tt.ok || id != tt.id || addr != tt.addr {
			t.Errorf("parseNotifyPayload(%q) = (%d,%q,%v), want (%d,%q,%v)",
				tt.payload, id, addr, ok, tt.id, tt.addr, tt.ok)
		}
	}
}

func TestHubRoute_SkipsClassificationWhenNoPushAndNoClients(t *testing.T) {
	var reactCalls, editCalls int
	hub := newTestHub()
	hub.reactionSubject = func(_ context.Context, _ int64) (int64, bool, error) {
		reactCalls++
		return 0, false, nil
	}
	hub.editSubject = func(_ context.Context, _ int64) (int64, bool, error) {
		editCalls++
		return 0, false, nil
	}

	hub.route(context.Background(), "new_msg", 42, "@offline@x")
	if reactCalls != 0 || editCalls != 0 {
		t.Fatalf("classification called for offline user with push disabled: react=%d edit=%d", reactCalls, editCalls)
	}
}

func TestHubRoute_CachesClassificationAcrossRecipients(t *testing.T) {
	var reactCalls, editCalls int
	hub := newTestHub()
	hub.reactionSubject = func(_ context.Context, id int64) (int64, bool, error) {
		reactCalls++
		if id == 100 {
			return 10, true, nil
		}
		return 0, false, nil
	}
	hub.editSubject = func(_ context.Context, _ int64) (int64, bool, error) {
		editCalls++
		return 0, false, nil
	}

	alice := newTestClient("@alice@x")
	bob := newTestClient("@bob@x")
	carol := newTestClient("@carol@x")
	hub.Register(alice)
	hub.Register(bob)
	hub.Register(carol)

	for _, addr := range []string{"@alice@x", "@bob@x", "@carol@x"} {
		hub.route(context.Background(), "new_msg", 100, addr)
	}
	if reactCalls != 1 || editCalls != 0 {
		t.Fatalf("reaction classification calls = (%d, %d), want (1, 0)", reactCalls, editCalls)
	}

	// Normal message across multiple recipients also classifies only once.
	for _, addr := range []string{"@alice@x", "@bob@x", "@carol@x"} {
		hub.route(context.Background(), "new_msg", 200, addr)
	}
	if reactCalls != 2 || editCalls != 1 {
		t.Fatalf("normal classification calls = (%d, %d), want (2, 1)", reactCalls, editCalls)
	}

	// Cache ring remains strictly bounded to msgClassCacheSize.
	var cache msgClassCache
	for i := int64(1); i <= msgClassCacheSize+50; i++ {
		cache.put(i, msgClassification{kind: msgClassNormal})
	}
	if got := len(cache.m); got != msgClassCacheSize {
		t.Fatalf("msgClassCache size = %d, want %d", got, msgClassCacheSize)
	}
	if _, ok := cache.get(1); ok {
		t.Fatal("oldest entry should have been evicted from msgClassCache")
	}
	if _, ok := cache.get(msgClassCacheSize + 50); !ok {
		t.Fatal("newest entry should still be in msgClassCache")
	}
}

func TestHubRoute_DoesNotCacheTransientClassificationError(t *testing.T) {
	var reactCalls, editCalls int
	transientErr := errors.New("transient db timeout")

	hub := newTestHub()
	hub.reactionSubject = func(_ context.Context, id int64) (int64, bool, error) {
		reactCalls++
		if id == 300 {
			if reactCalls == 1 {
				return 0, false, transientErr
			}
			return 10, true, nil
		}
		return 0, false, nil
	}
	hub.editSubject = func(_ context.Context, id int64) (int64, bool, error) {
		editCalls++
		if id == 400 {
			if editCalls == 1 {
				return 0, false, transientErr
			}
			return 20, true, nil
		}
		return 0, false, nil
	}

	alice := newTestClient("@alice@x")
	bob := newTestClient("@bob@x")
	carol := newTestClient("@carol@x")
	hub.Register(alice)
	hub.Register(bob)
	hub.Register(carol)

	// First recipient hits a transient reaction classification error: must NOT cache.
	hub.route(context.Background(), "new_msg", 300, "@alice@x")
	// Second recipient re-runs classification, succeeds as a reaction, and caches.
	hub.route(context.Background(), "new_msg", 300, "@bob@x")
	// Third recipient uses the cached reaction classification.
	hub.route(context.Background(), "new_msg", 300, "@carol@x")

	if reactCalls != 2 {
		t.Fatalf("reactionSubject calls = %d, want 2 (retry after transient error, then cache)", reactCalls)
	}
	for _, c := range []*wsClient{bob, carol} {
		select {
		case raw := <-c.send:
			var env struct {
				Type string          `json:"type"`
				Data messageListItem `json:"data"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Type != eventReaction || env.Data.ID != 10 {
				t.Fatalf("%s got (%s, %d), want (%s, 10)", c.addr, env.Type, env.Data.ID, eventReaction)
			}
		default:
			t.Fatalf("%s did not receive reaction event", c.addr)
		}
	}

	// Drain alice's fallback frame from the transient error.
	<-alice.send

	// Transient edit classification error is also not cached.
	editCalls = 0
	hub.route(context.Background(), "new_msg", 400, "@alice@x")
	hub.route(context.Background(), "new_msg", 400, "@bob@x")
	hub.route(context.Background(), "new_msg", 400, "@carol@x")
	if editCalls != 2 {
		t.Fatalf("editSubject calls = %d, want 2 (retry after transient error, then cache)", editCalls)
	}
	for _, c := range []*wsClient{bob, carol} {
		select {
		case raw := <-c.send:
			var env struct {
				Type string          `json:"type"`
				Data messageListItem `json:"data"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if env.Type != eventEdit || env.Data.ID != 20 {
				t.Fatalf("%s got (%s, %d), want (%s, 20)", c.addr, env.Type, env.Data.ID, eventEdit)
			}
		default:
			t.Fatalf("%s did not receive edit event", c.addr)
		}
	}
}

func TestHubPushWorkerPool_BoundedConcurrencyAndDrain(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 2
	hub.pushQueueSize = 16

	const total = 8
	allDone := make(chan struct{})
	var inFlight, maxInFlight, completed syncatomic.Int32
	hub.SetPushNotifier(func(_ context.Context, _ int64, _ string) {
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inFlight.Add(-1)
		if completed.Add(1) == total {
			close(allDone)
		}
	})

	for i := int64(1); i <= total; i++ {
		hub.route(context.Background(), "new_msg", i, "@offline@x")
	}
	select {
	case <-allDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for pushes: completed=%d/%d", completed.Load(), total)
	}
	hub.Close()

	if got := completed.Load(); got != total {
		t.Fatalf("completed pushes = %d, want %d", got, total)
	}
	if got := maxInFlight.Load(); got > 2 {
		t.Fatalf("max concurrent pushes = %d, exceeds worker pool bound 2", got)
	}
}

func TestHubCloseContext_CancelsQueuedAndInFlightPushes(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 2
	hub.pushQueueSize = 4

	started := make(chan struct{}, 2)
	var called, cancelled syncatomic.Int32
	hub.SetPushNotifier(func(ctx context.Context, _ int64, _ string) {
		called.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		cancelled.Add(1)
	})

	// Fill both workers, fill the 4-slot queue, and trigger 2 drops.
	for i := int64(1); i <= 8; i++ {
		hub.route(context.Background(), "new_msg", i, "@offline@x")
		if i == 2 {
			// Wait until both workers have pulled their jobs before filling the queue.
			<-started
			<-started
		}
	}
	if got := hub.PushDrops(); got != 2 {
		t.Fatalf("PushDrops() = %d, want 2", got)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := hub.CloseContext(closeCtx); err != nil {
		t.Fatalf("CloseContext returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("CloseContext took %s, expected prompt cancellation well before deadline", elapsed)
	}
	if got := called.Load(); got != 2 {
		t.Fatalf("notifyPush called %d times, want 2 (queued jobs should be skipped after cancel)", got)
	}
	if got := cancelled.Load(); got != 2 {
		t.Fatalf("in-flight pushes observing cancellation = %d, want 2", got)
	}
}

func TestHubCloseContext_RespectsDeadlineWhenWorkerBlocks(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 1
	hub.pushQueueSize = 1

	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hub.SetPushNotifier(func(_ context.Context, _ int64, _ string) {
		close(started)
		<-release
	})

	hub.route(context.Background(), "new_msg", 1, "@offline@x")
	<-started

	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := hub.CloseContext(closeCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext err = %v, want context.DeadlineExceeded", err)
	}
}

func TestHubDrainContext_SendsQueuedPushes(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 2
	hub.pushQueueSize = 16

	const total = 8
	var completed, cancelled syncatomic.Int32
	hub.SetPushNotifier(func(ctx context.Context, _ int64, _ string) {
		time.Sleep(15 * time.Millisecond)
		if ctx.Err() != nil {
			cancelled.Add(1)
			return
		}
		completed.Add(1)
	})
	for i := int64(1); i <= total; i++ {
		hub.route(context.Background(), "new_msg", i, "@offline@x")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hub.DrainContext(drainCtx); err != nil {
		t.Fatalf("DrainContext returned error: %v", err)
	}
	if got := completed.Load(); got != total {
		t.Fatalf("pushes sent = %d, want all %d queued at shutdown (cancelled: %d)", got, total, cancelled.Load())
	}

	// The pool is closed: a later notification is not queued.
	hub.route(context.Background(), "new_msg", total+1, "@offline@x")
	time.Sleep(30 * time.Millisecond)
	if got := completed.Load(); got != total {
		t.Fatalf("a push was sent after the drain: %d", got)
	}
}

func TestHubDrainContext_CancelsWhatIsLeftAtTheDeadline(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 1
	hub.pushQueueSize = 4

	started := make(chan struct{}, 1)
	var called, cancelled syncatomic.Int32
	hub.SetPushNotifier(func(ctx context.Context, _ int64, _ string) {
		called.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		cancelled.Add(1)
	})
	hub.route(context.Background(), "new_msg", 1, "@offline@x")
	<-started
	hub.route(context.Background(), "new_msg", 2, "@offline@x")

	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := hub.DrainContext(drainCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DrainContext err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > pushAbortGrace {
		t.Fatalf("DrainContext took %s; workers should exit promptly once cancelled", elapsed)
	}
	if got := cancelled.Load(); got != 1 {
		t.Fatalf("in-flight pushes observing cancellation = %d, want 1", got)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("notifyPush called %d times, want 1 (the queued job is skipped after cancel)", got)
	}
}
