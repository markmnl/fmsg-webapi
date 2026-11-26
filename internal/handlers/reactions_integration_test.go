package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// An add-to batch is a message of its own: a reaction names the message or
// the batch it was made on, goes to that message's participants, and is shown
// on it alone.
func TestReactionsPerMessage(t *testing.T) {
	a := newFinalizationAPI(t)
	alice, bob, carol := "@alice@example.com", "@bob@example.com", "@carol@example.com"
	orig := a.sendAs(t, alice, []string{bob}, "hello", nil)
	batch := jsonResponse(t, a.request(alice, "POST", "/fmsg/"+orig+"/add-to", map[string]any{"add_to": []string{carol}}), 200)
	batchHash := batch["sha256"].(string)

	react := func(who, id, emoji string, code int) string {
		t.Helper()
		w := a.request(who, "POST", "/fmsg/"+id+"/react", map[string]any{"emoji": emoji})
		if w.Code != code {
			t.Fatalf("%s reacts %s on %s: HTTP %d, want %d: %s", who, emoji, id, w.Code, code, w.Body)
		}
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return fmt.Sprintf("%.0f", v["id"])
	}
	to := func(id string) []string {
		t.Helper()
		var out []string
		rows, err := a.pool.Query(context.Background(), `SELECT addr FROM msg_to WHERE msg_id = $1`, mustID(t, id))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		sort.Strings(out)
		return out
	}

	// On the message: its participants only.
	if got := to(react(bob, orig, "👍", 201)); fmt.Sprint(got) != fmt.Sprint([]string{alice}) {
		t.Errorf("reaction on the message went to %v", got)
	}
	// On the batch: its participants, the message's and those it added. The
	// same emoji on another message is a reaction of its own, not a repeat.
	if got := to(react(bob, batchHash, "👍", 201)); fmt.Sprint(got) != fmt.Sprint([]string{alice, carol}) {
		t.Errorf("reaction on the batch went to %v", got)
	}
	react(bob, batchHash, "👍", 200) // the same again on the batch: nothing sent
	// Someone only added holds the batch: named by id, that is what they react to.
	if got := to(react(carol, orig, "🎉", 201)); fmt.Sprint(got) != fmt.Sprint([]string{alice, bob}) {
		t.Errorf("added recipient's reaction went to %v", got)
	}

	var got struct {
		Reactions []struct {
			Emoji string   `json:"emoji"`
			From  []string `json:"from"`
		} `json:"reactions"`
		AddTo []struct {
			SHA256    string `json:"sha256"`
			Reactions []struct {
				Emoji string   `json:"emoji"`
				From  []string `json:"from"`
			} `json:"reactions"`
		} `json:"add_to"`
	}
	w := a.request(alice, "GET", "/fmsg/"+orig, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if fmt.Sprint(got.Reactions) != fmt.Sprint([]any{struct {
		Emoji string
		From  []string
	}{"👍", []string{bob}}}) {
		t.Errorf("message reactions = %+v", got.Reactions)
	}
	if len(got.AddTo) != 1 || got.AddTo[0].SHA256 != batchHash || fmt.Sprint(got.AddTo[0].Reactions) != fmt.Sprint([]any{
		struct {
			Emoji string
			From  []string
		}{"👍", []string{bob}},
		struct {
			Emoji string
			From  []string
		}{"🎉", []string{carol}},
	}) {
		t.Errorf("batch reactions = %+v", got.AddTo)
	}
}
