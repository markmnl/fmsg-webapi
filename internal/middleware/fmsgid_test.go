package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCheckFmsgIDRejectsInvalidResponse(t *testing.T) {
	for _, body := range []string{"", "<html>upstream error</html>", `{"acceptingNew":"true"}`, `{`, `{}`, `null`, `{"acceptingNew":null}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			_, accepting, err := CheckFmsgID(server.URL, "@alice@example.com")
			if err == nil || accepting {
				t.Fatalf("invalid response: accepting=%v, err=%v", accepting, err)
			}
		})
	}
}

func TestCheckFmsgIDSeparatesServices(t *testing.T) {
	accepting := fmsgIDServer(t, http.StatusOK, true)
	defer accepting.Close()
	disabled := fmsgIDServer(t, http.StatusOK, false)
	defer disabled.Close()
	const addr = "@service-cache@example.com"
	if _, ok, err := CheckFmsgID(accepting.URL, addr); err != nil || !ok {
		t.Fatalf("accepting service: accepting=%v, err=%v", ok, err)
	}
	if _, ok, err := CheckFmsgID(disabled.URL, addr); err != nil || ok {
		t.Fatalf("disabled service: accepting=%v, err=%v", ok, err)
	}
}

func TestRegisterLinkedFmsgIDRefreshesExistingAddress(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const addr = "@alice_bot@example.com"
			var accepting atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/fmsgid":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["address"] != addr ||
						body["accepting_new"] != true || body["quota_pool"] != "@alice@example.com" {
						t.Errorf("registration payload: %v, err=%v", body, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					accepting.Store(true)
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"created":true,"reinstated":false}`))
				case r.Method == http.MethodGet && r.URL.Path == "/fmsgid/"+addr:
					_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": accepting.Load()})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			if _, ok, err := CheckFmsgID(server.URL, addr); err != nil || ok {
				t.Fatalf("initial lookup: accepting=%v, err=%v", ok, err)
			}
			if reinstated, err := RegisterLinkedFmsgID(server.URL+"/", addr, "@alice@example.com"); err != nil || reinstated {
				t.Fatalf("registration: reinstated=%v, err=%v", reinstated, err)
			}
			if _, ok, err := CheckFmsgID(server.URL, addr); err != nil || !ok {
				t.Fatalf("lookup after registration: accepting=%v, err=%v", ok, err)
			}
		})
	}
}

func TestRegisterLinkedFmsgIDResponses(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		body           string
		wantReinstated bool
		wantErr        bool
	}{
		{name: "created", status: http.StatusCreated, body: `{"created":true,"reinstated":false}`},
		{name: "existing", status: http.StatusOK, body: `{"created":false,"reinstated":false}`},
		{name: "reinstated", status: http.StatusOK, body: `{"created":false,"reinstated":true}`, wantReinstated: true},
		{name: "fmsgid without accepting_new", status: http.StatusOK, body: ""},
		{name: "malformed", status: http.StatusOK, body: "<html>", wantErr: true},
		{name: "quota pool refused", status: http.StatusUnprocessableEntity, body: `{"error":"quota_pool address not found"}`, wantErr: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			reinstated, err := RegisterLinkedFmsgID(server.URL, "@alice_bot@example.com", "@alice@example.com")
			if (err != nil) != tc.wantErr || reinstated != tc.wantReinstated {
				t.Fatalf("reinstated=%v, err=%v; want reinstated=%v, error=%v", reinstated, err, tc.wantReinstated, tc.wantErr)
			}
		})
	}
}

func TestDisableFmsgID(t *testing.T) {
	const addr = "@alice_old@example.com"
	var accepting atomic.Bool
	accepting.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/fmsgid":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["address"] != addr ||
				body["accepting_new"] != false || body["quota_pool"] != nil {
				t.Errorf("disable payload: %v, err=%v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			accepting.Store(false)
			_, _ = w.Write([]byte(`{"created":false,"reinstated":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/fmsgid/"+addr:
			_ = json.NewEncoder(w).Encode(map[string]bool{"acceptingNew": accepting.Load()})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	if _, ok, err := CheckFmsgID(server.URL, addr); err != nil || !ok {
		t.Fatalf("initial lookup: accepting=%v, err=%v", ok, err)
	}
	if err := DisableFmsgID(server.URL, addr); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := CheckFmsgID(server.URL, addr); err != nil || ok {
		t.Fatalf("lookup after disabling (cache must be dropped): accepting=%v, err=%v", ok, err)
	}
}

// An fmsgid that ignores accepting_new answers without a body; disabling must
// not be reported as done.
func TestDisableFmsgIDFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"fmsgid without accepting_new", http.StatusOK, ""},
		{"unavailable", http.StatusServiceUnavailable, ""},
		{"forbidden", http.StatusForbidden, `{"error":"domain not active"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			if err := DisableFmsgID(server.URL, "@alice_bot@example.com"); err == nil {
				t.Fatal("disable reported success")
			}
		})
	}
}

func TestCheckFmsgIDEscapesAddress(t *testing.T) {
	const addr = "@alice?bot#1%tag@example.com"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fmsgid/"+addr || r.URL.RawQuery != "" {
			t.Errorf("lookup changed address: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"acceptingNew":true}`))
	}))
	defer server.Close()
	if code, accepting, err := CheckFmsgID(server.URL, addr); err != nil || code != http.StatusOK || !accepting {
		t.Fatalf("lookup: code=%d, accepting=%v, err=%v", code, accepting, err)
	}
}
