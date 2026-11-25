package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestIndicatorThrottle_SweepThrottled(t *testing.T) {
	th := &indicatorThrottle{last: make(map[string]time.Time, 10005)}
	t0 := time.Unix(1000, 0)
	// Seed >10000 active entries at t0.
	for i := range 10002 {
		th.last[fmt.Sprintf("u%d|1", i)] = t0
	}

	// First call triggers sweep (none expired yet) and records lastSweep = t0+100ms.
	t1 := t0.Add(100 * time.Millisecond)
	if !th.allow("fresh1|1", t1) {
		t.Fatal("fresh1 refused")
	}
	if !th.lastSweep.Equal(t1) {
		t.Fatalf("lastSweep = %v, want %v", th.lastSweep, t1)
	}

	// At t0 + indicatorMinInterval, the original 10002 entries are expired, but
	// less than indicatorMinInterval has passed since lastSweep (t1), so sweep
	// must NOT run yet.
	t2 := t0.Add(indicatorMinInterval)
	if !th.allow("fresh2|1", t2) {
		t.Fatal("fresh2 refused")
	}
	if len(th.last) < 10000 {
		t.Fatalf("sweep ran prematurely before indicatorMinInterval elapsed since lastSweep: len=%d", len(th.last))
	}

	// Once indicatorMinInterval has elapsed since lastSweep, sweep runs and
	// evicts the expired entries.
	t3 := t1.Add(indicatorMinInterval)
	if !th.allow("fresh3|1", t3) {
		t.Fatal("fresh3 refused")
	}
	if len(th.last) > 5 {
		t.Fatalf("sweep did not evict expired entries after interval: len=%d", len(th.last))
	}
}

func TestHubRouteIndicator_SkipsThreadRootWhenNoParticipantConnected(t *testing.T) {
	hub := newTestHub()
	rootCalls := 0
	hub.indicatorParticipants = func(_ context.Context, _ int64) ([]string, error) {
		return []string{"@alice@x", "@bob@x"}, nil
	}
	hub.threadRoot = func(_ context.Context, _ int64) (int64, error) {
		rootCalls++
		return 3, nil
	}

	// Only an unrelated user (dave) is connected: threadRoot must be skipped.
	dave := newTestClient("@dave@x")
	hub.Register(dave)
	hub.routeIndicator(context.Background(), `{"id":7,"from":"@bob@x","emoji":"✍️","ttl":6}`)
	if rootCalls != 0 {
		t.Fatalf("threadRoot called %d times when no participant connected, want 0", rootCalls)
	}

	// Once alice connects, threadRoot is queried and alice receives the indicator.
	alice := newTestClient("@alice@x")
	hub.Register(alice)
	hub.routeIndicator(context.Background(), `{"id":7,"from":"@bob@x","emoji":"✍️","ttl":6}`)
	if rootCalls != 1 {
		t.Fatalf("threadRoot called %d times, want 1", rootCalls)
	}
	select {
	case payload := <-alice.send:
		var env struct {
			Type string    `json:"type"`
			Data Indicator `json:"data"`
		}
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != eventIndicator || env.Data.RootID != 3 {
			t.Fatalf("unexpected indicator envelope: %+v", env)
		}
	default:
		t.Fatal("alice did not receive indicator")
	}
}
