package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFetchFmsgIDDetail(t *testing.T) {
	const addr = "@detail@example.com"
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/fmsgid/"+addr {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gets.Add(1)
		_, _ = w.Write([]byte(`{"address":"` + addr + `","acceptingNew":true,"limitRecvSizeTotal":1000,"limitRecvCountPer1d":10,` +
			`"limitSendCountPer1d":5,"recvSizeTotal":100,"recvCountPer1d":3,"sendCountPer1d":4,"sendSizeTotal":77}`))
	}))
	defer server.Close()

	for i := 1; i <= 2; i++ {
		detail, status, err := FetchFmsgIDDetail(server.URL, addr)
		if err != nil || status != http.StatusOK {
			t.Fatalf("fetch: status=%d err=%v", status, err)
		}
		if int(gets.Load()) != i {
			t.Fatalf("fetch %d made %d requests: the detail must not come from the cache", i, gets.Load())
		}
		want := FmsgIDDetail{
			AcceptingNew:       true,
			LimitRecvSizeTotal: 1000, LimitRecvCountPer1d: 10, LimitSendCountPer1d: 5,
			// limits fmsgid leaves out are unlimited
			LimitRecvSizePer1d: -1, LimitSendSizeTotal: -1, LimitSendSizePerMsg: -1, LimitSendSizePer1d: -1,
			RecvSizeTotal: 100, RecvCountPer1d: 3, SendCountPer1d: 4, SendSizeTotal: 77,
		}
		if *detail != want {
			t.Fatalf("detail = %+v, want %+v", *detail, want)
		}
	}

	// A fresh detail also refreshes the lookup cache CheckFmsgID uses.
	before := gets.Load()
	if code, accepting, err := CheckFmsgID(server.URL, addr); err != nil || code != http.StatusOK || !accepting {
		t.Fatalf("cached check: code=%d accepting=%v err=%v", code, accepting, err)
	}
	if gets.Load() != before {
		t.Fatal("CheckFmsgID did not use the lookup cache refreshed by FetchFmsgIDDetail")
	}

	if detail, status, err := FetchFmsgIDDetail(server.URL, "@missing@example.com"); err != nil || status != http.StatusNotFound || detail != nil {
		t.Fatalf("missing address: detail=%v status=%d err=%v", detail, status, err)
	}
}

func TestFetchFmsgIDDetailRejectsInvalidResponse(t *testing.T) {
	for _, body := range []string{"", "<html>", `{}`, `{"acceptingNew":null}`, `{"acceptingNew":true,"recvSizeTotal":"x"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			if detail, _, err := FetchFmsgIDDetail(server.URL, "@alice@example.com"); err == nil || detail != nil {
				t.Fatalf("invalid response accepted: detail=%v err=%v", detail, err)
			}
		})
	}
}

func TestRecordFmsgIDUsage(t *testing.T) {
	type usage struct {
		Address string  `json:"address"`
		Ts      float64 `json:"ts"`
		Size    int64   `json:"size"`
	}
	got := map[string]usage{}
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u usage
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
			t.Errorf("decode: %v", err)
		}
		got[r.URL.Path] = u
		w.WriteHeader(status)
	}))
	defer server.Close()

	if err := RecordFmsgIDRecv(server.URL+"/", "@bob@example.com", 1700000000.5, 1234); err != nil {
		t.Fatal(err)
	}
	if err := RecordFmsgIDSend(server.URL, "@alice@example.com", 1700000001, 99); err != nil {
		t.Fatal(err)
	}
	if u := got["/fmsgid/recv"]; u != (usage{"@bob@example.com", 1700000000.5, 1234}) {
		t.Fatalf("recv usage = %+v", u)
	}
	if u := got["/fmsgid/send"]; u != (usage{"@alice@example.com", 1700000001, 99}) {
		t.Fatalf("send usage = %+v", u)
	}

	status = http.StatusInternalServerError
	if err := RecordFmsgIDSend(server.URL, "@alice@example.com", 1, 1); err == nil {
		t.Fatal("a failed record was not reported")
	}
}
