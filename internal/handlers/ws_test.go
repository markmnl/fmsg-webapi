package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestActAsParam(t *testing.T) {
	tests := []struct {
		name   string
		header string
		query  string
		want   string
	}{
		{name: "neither set", want: ""},
		{name: "header only", header: "@bot@example.com", want: "@bot@example.com"},
		{name: "query only", query: "@bot@example.com", want: "@bot@example.com"},
		{name: "header wins over query", header: "@a@example.com", query: "@b@example.com", want: "@a@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			target := "/fmsg/ws"
			if tt.query != "" {
				target += "?act_as=" + url.QueryEscape(tt.query)
			}
			c.Request = httptest.NewRequest("GET", target, nil)
			if tt.header != "" {
				c.Request.Header.Set("X-FMSG-Act-As", tt.header)
			}
			if got := actAsParam(c); got != tt.want {
				t.Errorf("actAsParam() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWSCheckOrigin(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		origin  string
		want    bool
	}{
		{name: "no origin header allowed", allowed: []string{"https://app.example.com"}, origin: "", want: true},
		{name: "empty allowlist allows any", allowed: nil, origin: "https://any.example.com", want: true},
		{name: "exact match allowed", allowed: []string{"https://app.example.com"}, origin: "https://app.example.com", want: true},
		{name: "unlisted origin rejected", allowed: []string{"https://app.example.com"}, origin: "https://evil.example.com", want: false},
		{name: "wildcard allows any origin", allowed: []string{"*"}, origin: "https://anywhere.example.com", want: true},
		{name: "wildcard among origins allows any", allowed: []string{"https://a.example.com", "*"}, origin: "https://b.example.com", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewWSHandler(nil, nil, tt.allowed)
			req := httptest.NewRequest(http.MethodGet, "/fmsg/ws", nil)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if got := h.upgrader.CheckOrigin(req); got != tt.want {
				t.Errorf("CheckOrigin(%q) with allowed=%v = %v, want %v", tt.origin, tt.allowed, got, tt.want)
			}
		})
	}
}

func TestWSWritePumpClosesClientOnWriteError(t *testing.T) {
	clientCh := make(chan *wsClient, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c := &wsClient{
			conn: conn,
			addr: "@alice@example.com",
			send: make(chan []byte, 4),
			done: make(chan struct{}),
		}
		clientCh <- c
		c.writePump()
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	peer, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := <-clientCh

	// Close both ends' underlying socket so the server's next write fails.
	_ = peer.Close()
	_ = c.conn.UnderlyingConn().Close()
	c.send <- []byte(`{"type":"new_msg"}`)

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("writePump did not close client on write error")
	}
}
