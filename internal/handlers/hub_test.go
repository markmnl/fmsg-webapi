package handlers

import (
	"context"
	"encoding/json"
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
	hub.reactionSubject = func(_ context.Context, _ int64) (int64, bool) {
		reactCalls++
		return 0, false
	}
	hub.editSubject = func(_ context.Context, _ int64) (int64, bool) {
		editCalls++
		return 0, false
	}

	hub.route(context.Background(), "new_msg", 42, "@offline@x")
	if reactCalls != 0 || editCalls != 0 {
		t.Fatalf("classification called for offline user with push disabled: react=%d edit=%d", reactCalls, editCalls)
	}
}

func TestHubRoute_CachesClassificationAcrossRecipients(t *testing.T) {
	var reactCalls, editCalls int
	hub := newTestHub()
	hub.reactionSubject = func(_ context.Context, id int64) (int64, bool) {
		reactCalls++
		if id == 100 {
			return 10, true
		}
		return 0, false
	}
	hub.editSubject = func(_ context.Context, _ int64) (int64, bool) {
		editCalls++
		return 0, false
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

func TestHubPushWorkerPool_BoundedConcurrencyAndDrain(t *testing.T) {
	hub := newTestHub()
	hub.pushWorkers = 2
	hub.pushQueueSize = 16

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
		completed.Add(1)
	})

	const total = 8
	for i := int64(1); i <= total; i++ {
		hub.route(context.Background(), "new_msg", i, "@offline@x")
	}
	hub.Close()

	if got := completed.Load(); got != total {
		t.Fatalf("completed pushes = %d, want %d (Close must drain worker pool)", got, total)
	}
	if got := maxInFlight.Load(); got > 2 {
		t.Fatalf("max concurrent pushes = %d, exceeds worker pool bound 2", got)
	}
}
