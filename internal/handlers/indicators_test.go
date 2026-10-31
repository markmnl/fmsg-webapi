package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestHubRouteIndicator(t *testing.T) {
	hub := newTestHub()
	hub.indicatorAudience = func(_ context.Context, id int64) ([]string, int64, error) {
		if id != 7 {
			return nil, 0, errors.New("unknown")
		}
		return []string{"@Alice@x", "@bob@x", "@carol@y"}, 3, nil
	}
	alice, bob, dave := newTestClient("@alice@x"), newTestClient("@bob@x"), newTestClient("@dave@x")
	for _, c := range []*wsClient{alice, bob, dave} {
		hub.Register(c)
	}

	hub.routeIndicator(context.Background(), `{"id":7,"from":"@BOB@x","emoji":"✍️","ttl":6}`)

	select {
	case payload := <-alice.send:
		var env struct {
			Type string    `json:"type"`
			Data Indicator `json:"data"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		want := Indicator{ID: 7, RootID: 3, From: "@BOB@x", Emoji: "✍️", TTL: 6}
		if env.Type != eventIndicator || env.Data != want {
			t.Errorf("alice got %s %+v, want %+v", env.Type, env.Data, want)
		}
	default:
		t.Fatal("alice, a participant, should get the indicator")
	}
	for name, c := range map[string]*wsClient{"bob (the indicating participant)": bob, "dave (not a participant)": dave} {
		select {
		case <-c.send:
			t.Errorf("%s must not get the indicator", name)
		default:
		}
	}

	// Malformed payloads and unknown messages are dropped.
	for _, p := range []string{`nope`, `{"id":0,"from":"@b@x"}`, `{"id":7}`, `{"id":8,"from":"@b@x","emoji":"x","ttl":1}`} {
		hub.routeIndicator(context.Background(), p)
	}
	select {
	case <-alice.send:
		t.Error("a malformed or unknown indicator was delivered")
	default:
	}
}

func TestIndicatorThrottle(t *testing.T) {
	th := &indicatorThrottle{last: map[string]time.Time{}}
	now := time.Unix(1000, 0)
	if !th.allow("a|1", now) {
		t.Fatal("first indicator refused")
	}
	if th.allow("a|1", now.Add(500*time.Millisecond)) {
		t.Error("second within the interval allowed")
	}
	if !th.allow("a|2", now.Add(500*time.Millisecond)) {
		t.Error("another message throttled")
	}
	if !th.allow("a|1", now.Add(indicatorMinInterval)) {
		t.Error("allowed again after the interval")
	}
}
