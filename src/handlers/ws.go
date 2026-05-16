package handlers

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/markmnl/fmsg-webapi/middleware"
)

// WebSocket tuning constants.
const (
	// wsSendBuffer is the per-client outbound queue depth. A client whose
	// queue fills (too slow to keep up) is disconnected.
	wsSendBuffer = 16
	// wsWriteWait is the time allowed to write a single frame.
	wsWriteWait = 10 * time.Second
	// wsPongWait is how long the server waits for any client frame (a pong
	// counts) before considering the connection dead.
	wsPongWait = 60 * time.Second
	// wsPingPeriod is how often the server pings the client. Must be less
	// than wsPongWait.
	wsPingPeriod = 45 * time.Second
	// wsMaxMessageSize caps inbound client frames; clients are not expected
	// to send anything meaningful, so this is small.
	wsMaxMessageSize = 1024
)

// WSHandler upgrades HTTP requests to WebSocket connections and registers them
// with the Hub. It authenticates with the same JWT verifier as the REST API.
type WSHandler struct {
	verifier *middleware.Verifier
	hub      *Hub
	upgrader websocket.Upgrader
}

// NewWSHandler builds a WSHandler. allowedOrigins is the list of browser
// origins permitted to open a WebSocket; an empty list allows any origin,
// mirroring the "CORS disabled" development behaviour in main.go.
func NewWSHandler(verifier *middleware.Verifier, hub *Hub, allowedOrigins []string) *WSHandler {
	return &WSHandler{
		verifier: verifier,
		hub:      hub,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					// Non-browser client (no Origin header).
					return true
				}
				if len(allowedOrigins) == 0 {
					return true
				}
				for _, o := range allowedOrigins {
					if o == origin {
						return true
					}
				}
				log.Printf("ws: rejected upgrade from origin %q", origin)
				return false
			},
		},
	}
}

// Connect handles GET /fmsg/ws. It authenticates the request (via the
// access_token query parameter or an Authorization header), upgrades the
// connection, and runs it until either side closes.
func (h *WSHandler) Connect(c *gin.Context) {
	token := c.Query("access_token")
	if token == "" {
		token = bearerToken(c.GetHeader("Authorization"))
	}
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing access token"})
		return
	}

	addr, status, msg := h.verifier.Authenticate(token)
	if status != http.StatusOK {
		c.JSON(status, gin.H{"error": msg})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		// Upgrade has already written an HTTP error response.
		log.Printf("ws: upgrade failed for %s: %v", addr, err)
		return
	}

	client := &wsClient{
		conn: conn,
		addr: strings.ToLower(addr),
		send: make(chan []byte, wsSendBuffer),
		done: make(chan struct{}),
	}
	h.hub.Register(client)

	go client.writePump()
	client.readPump() // blocks until the connection ends

	h.hub.Unregister(client)
	client.close()
}

// wsClient is a single connected WebSocket. All writes happen in writePump and
// all reads in readPump, satisfying gorilla/websocket's one-reader/one-writer
// concurrency requirement.
type wsClient struct {
	conn *websocket.Conn
	// addr is the authenticated user address, lower-cased, used as the Hub
	// registry key.
	addr string
	send chan []byte
	done chan struct{}

	closeOnce sync.Once
}

// close shuts the connection down exactly once, unblocking both pumps.
func (c *wsClient) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

// writePump is the sole writer: it drains the send queue and emits periodic
// pings to keep the connection alive and detect dead peers.
func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case payload := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// readPump is the sole reader. Clients are not expected to send application
// data; reading drives pong handling and detects disconnects. It returns when
// the connection closes.
func (c *wsClient) readPump() {
	c.conn.SetReadLimit(wsMaxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header value, returning "" when the header is absent or malformed.
func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
