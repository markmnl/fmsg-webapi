package handlers

import (
	"encoding/json"
	"reflect"
	"testing"
)

// inboxIDs returns the ids in identity's inbox, in listed order.
func (a *finalizationAPI) inboxIDs(t *testing.T, identity, query string) []string {
	t.Helper()
	w := a.request(identity, "GET", "/fmsg"+query, nil)
	if w.Code != 200 {
		t.Fatalf("list for %s: %d %s", identity, w.Code, w.Body)
	}
	var items []struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID.String()
	}
	return ids
}

// An older message an identity has just been added to is listed first: the
// inbox is ordered by when each message arrived for that identity, not by id.
func TestListOrdersByArrival(t *testing.T) {
	a := newFinalizationAPI(t)

	old := a.sendAs(t, bob, []string{alice}, "an older message", nil)
	ageBy(t, a, mustID(t, old), 3600)
	newer := a.sendAs(t, alice, []string{carol}, "a newer message", nil)
	ageBy(t, a, mustID(t, newer), 60)
	later := a.sendAs(t, bob, []string{alice}, "sent after the older one", nil)
	ageBy(t, a, mustID(t, later), 60)

	if got, want := a.inboxIDs(t, carol, ""), []string{newer}; !reflect.DeepEqual(got, want) {
		t.Fatalf("carol before add-to: %v, want %v", got, want)
	}

	jsonResponse(t, a.request(bob, "POST", "/fmsg/"+old+"/add-to", map[string]any{"add_to": []string{carol}}), 200)

	if got, want := a.inboxIDs(t, carol, ""), []string{old, newer}; !reflect.DeepEqual(got, want) {
		t.Errorf("carol after add-to: %v, want the message she was just added to first %v", got, want)
	}
	if got, want := a.inboxIDs(t, carol, "?limit=1"), []string{old}; !reflect.DeepEqual(got, want) {
		t.Errorf("carol first page: %v, want %v", got, want)
	}
	if got, want := a.inboxIDs(t, carol, "?limit=1&offset=1"), []string{newer}; !reflect.DeepEqual(got, want) {
		t.Errorf("carol second page: %v, want %v", got, want)
	}

	// Adding someone else doesn't move the message for its other recipients.
	if got, want := a.inboxIDs(t, alice, ""), []string{later, old}; !reflect.DeepEqual(got, want) {
		t.Errorf("alice: %v, want %v", got, want)
	}
}
