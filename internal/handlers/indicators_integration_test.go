package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// listenIndicators LISTENs on the indicator channel and returns a function
// that waits briefly for the next payload ("" when none arrives).
func listenIndicators(t *testing.T, pool *pgxpool.Pool) func() string {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(ctx, "LISTEN "+IndicatorChannel); err != nil {
		t.Fatal(err)
	}
	return func() string {
		wctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		if err != nil {
			return ""
		}
		return n.Payload
	}
}

func TestIndicateRoute(t *testing.T) {
	a := newFinalizationAPI(t)
	next := listenIndicators(t, a.pool)
	throttle = &indicatorThrottle{last: map[string]time.Time{}}

	id := a.sendAs(t, alice, []string{bob}, "hello", nil)
	reply := a.sendAs(t, bob, []string{alice}, "re", mustID(t, id))

	got := func(want Indicator) {
		t.Helper()
		var ind Indicator
		p := next()
		if err := json.Unmarshal([]byte(p), &ind); err != nil || ind != want {
			t.Errorf("notified %q, want %+v", p, want)
		}
	}

	if w := a.request(bob, "POST", "/fmsg/"+id+"/indicate", map[string]any{"emoji": "✍️"}); w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("indicate: HTTP %d %s", w.Code, w.Body)
	}
	got(Indicator{ID: mustID(t, id), From: bob, Emoji: "✍️", TTL: indicatorTTLDefault})

	// Within the interval: accepted, but nothing is published.
	if w := a.request(bob, "POST", "/fmsg/"+id+"/indicate", map[string]any{"emoji": "✍️"}); w.Code != http.StatusAccepted {
		t.Fatalf("throttled indicate: HTTP %d", w.Code)
	}
	if p := next(); p != "" {
		t.Errorf("throttled indicator published: %s", p)
	}
	// A clear is never throttled; ttl 0 clears too.
	a.request(bob, "POST", "/fmsg/"+id+"/indicate", map[string]any{"emoji": ""})
	got(Indicator{ID: mustID(t, id), From: bob})
	a.request(alice, "POST", "/fmsg/"+reply+"/indicate", map[string]any{"emoji": "⏳", "ttl": 0})
	got(Indicator{ID: mustID(t, reply), From: alice})
	// ttl is clamped.
	a.request(alice, "POST", "/fmsg/"+reply+"/indicate", map[string]any{"emoji": "⏳", "ttl": 500})
	got(Indicator{ID: mustID(t, reply), From: alice, Emoji: "⏳", TTL: indicatorTTLMax})

	for _, c := range []struct {
		name, who, id string
		body          map[string]any
		want          int
	}{
		{"not a participant", carol, id, map[string]any{"emoji": "✍️"}, http.StatusForbidden},
		{"not an emoji", bob, id, map[string]any{"emoji": "typing"}, http.StatusBadRequest},
		{"two emoji", bob, id, map[string]any{"emoji": "✍️✍️"}, http.StatusBadRequest},
		{"negative ttl", bob, id, map[string]any{"emoji": "✍️", "ttl": -1}, http.StatusBadRequest},
		{"unknown message", bob, "999999", map[string]any{"emoji": "✍️"}, http.StatusNotFound},
	} {
		if w := a.request(c.who, "POST", "/fmsg/"+c.id+"/indicate", c.body); w.Code != c.want {
			t.Errorf("%s: HTTP %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
	draft := a.draft(t, alice, "unsent", nil)
	if w := a.request(alice, "POST", "/fmsg/"+draft+"/indicate", map[string]any{"emoji": "✍️"}); w.Code != http.StatusConflict {
		t.Errorf("draft: HTTP %d, want 409", w.Code)
	}
	if p := next(); p != "" {
		t.Errorf("a refused indicator was published: %s", p)
	}

	// Nothing is stored: no table grew.
	var n int
	if err := a.pool.QueryRow(context.Background(), `SELECT count(*) FROM msg`).Scan(&n); err != nil || n != 3 {
		t.Errorf("messages after indicating: %d, %v (want 3: two sent, one draft)", n, err)
	}

	// The audience: every participant, and the thread's first message.
	addrs, root, err := a.h.indicatorAudience(context.Background(), mustID(t, reply))
	if err != nil || root != mustID(t, id) || len(addrs) != 2 {
		t.Errorf("audience of the reply: %v root %d, %v", addrs, root, err)
	}
}
