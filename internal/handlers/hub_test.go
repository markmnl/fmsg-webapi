package handlers

import (
	"context"
	"encoding/json"
	"testing"
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
