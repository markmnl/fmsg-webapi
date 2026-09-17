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

func TestRegisterFmsgIDRefreshesExistingAddress(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const addr = "@alice_bot@example.com"
			var accepting atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/fmsgid":
					var body struct{ Address string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Address != addr {
						t.Errorf("registration payload: address=%q, err=%v", body.Address, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					accepting.Store(true)
					w.WriteHeader(status)
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
			if err := RegisterFmsgID(server.URL+"/", addr); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := CheckFmsgID(server.URL, addr); err != nil || !ok {
				t.Fatalf("lookup after registration: accepting=%v, err=%v", ok, err)
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
