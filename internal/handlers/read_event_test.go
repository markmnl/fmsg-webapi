package handlers

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// A msg_read notification is pushed to the reader as a read event carrying
// the message.
func TestHubRoute_MsgReadPushesReadEvent(t *testing.T) {
	hub := &Hub{
		buildItem: func(_ context.Context, msgID int64, _ string) (*messageListItem, error) {
			return &messageListItem{ID: msgID, Read: true}, nil
		},
		registry: make(map[string]map[*wsClient]struct{}),
	}
	c := &wsClient{addr: "@bob@example.com", send: make(chan []byte, 4)}
	hub.Register(c)

	hub.route(context.Background(), "msg_read", 9, "@Bob@example.com")
	select {
	case frame := <-c.send:
		var env struct {
			Type string          `json:"type"`
			Data messageListItem `json:"data"`
		}
		if err := json.Unmarshal(frame, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != eventRead || env.Data.ID != 9 || !env.Data.Read {
			t.Errorf("got %s for %d (read %v), want %s for 9 (read)", env.Type, env.Data.ID, env.Data.Read, eventRead)
		}
	default:
		t.Fatal("no frame pushed")
	}
}

// Marking a message read notifies msg_read with the message and its reader,
// once: reading it again changes nothing and notifies nothing.
func TestMarkReadNotifies(t *testing.T) {
	a := newFinalizationAPI(t)
	const alice, bob = "@alice@example.com", "@bob@example.com"
	ctx := context.Background()
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN msg_read"); err != nil {
		t.Fatal(err)
	}

	id := a.draft(t, alice, "hello", nil)
	jsonResponse(t, a.request(alice, "POST", "/fmsg/"+id+"/send", map[string]any{}), 200)
	jsonResponse(t, a.request(bob, "POST", "/fmsg/"+id+"/read", map[string]any{}), 200)

	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(wait)
	if err != nil {
		t.Fatalf("no msg_read notification: %v", err)
	}
	if want := id + "," + bob; n.Payload != want {
		t.Errorf("payload %q, want %q", n.Payload, want)
	}

	jsonResponse(t, a.request(bob, "POST", "/fmsg/"+id+"/read", map[string]any{}), 200)
	again, cancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel2()
	if n, err := conn.Conn().WaitForNotification(again); err == nil {
		t.Errorf("second read notified %q", n.Payload)
	}
}
