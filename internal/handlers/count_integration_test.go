package handlers

import (
	"net/http"
	"testing"
)

func TestCountThreads(t *testing.T) {
	a := newFinalizationAPI(t)
	count := func(who, query string) float64 {
		t.Helper()
		return jsonResponse(t, a.request(who, "GET", "/fmsg/count"+query, nil), http.StatusOK)["threads"].(float64)
	}

	first := a.sendAs(t, alice, []string{bob}, "first thread", nil)
	a.sendAs(t, bob, []string{alice}, "a reply is the same thread", mustID(t, first))
	a.sendAs(t, alice, []string{bob, carol}, "second thread", nil)
	a.draft(t, bob, "a draft is not a thread", nil)

	for who, want := range map[string]float64{alice: 2, bob: 2, carol: 1} {
		if got := count(who, ""); got != want {
			t.Errorf("%s: %v threads, want %v", who, got, want)
		}
	}
	if got := count("@dave@example.com", ""); got != 0 {
		t.Errorf("an outsider: %v threads, want 0", got)
	}

	// A thread in the trash leaves the default count, and is counted by trash=only.
	jsonResponse(t, a.request(bob, "DELETE", "/fmsg/"+first+"?thread=true", nil), http.StatusOK)
	if got := count(bob, ""); got != 1 {
		t.Errorf("bob after trashing a thread: %v, want 1", got)
	}
	if got := count(bob, "?trash=only"); got != 1 {
		t.Errorf("bob's trash: %v, want 1", got)
	}
	if got := count(bob, "?trash=include"); got != 2 {
		t.Errorf("bob including trash: %v, want 2", got)
	}
	if w := a.request(bob, "GET", "/fmsg/count?trash=nope", nil); w.Code != http.StatusBadRequest {
		t.Errorf("bad trash value: HTTP %d", w.Code)
	}
}
