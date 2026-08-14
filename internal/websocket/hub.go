package websocket

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// MessageType identifies the type of websocket message
type MessageType string

const (
	MsgDeviceUpdate  MessageType = "device_update"
	MsgDeviceAdd     MessageType = "device_add"
	MsgDeviceAdded   MessageType = "device_added"
	MsgDeviceRemoved MessageType = "device_removed"
	MsgStatsUpdate   MessageType = "stats_update"
	MsgJobUpdate     MessageType = "job_update"
	MsgJobProgress   MessageType = "job_progress"
	MsgJobEvent      MessageType = "job_event"
	MsgPong          MessageType = "pong"
)

const (
	// WebSocket channel sizes tuned for large deployments.
	//
	// WaveControl can emit bursts of updates for thousands of devices; small
	// websocket buffers can drop important state transitions (e.g., online
	//→offline) leading to stale dashboards/host panels until a full refresh.
	wsBroadcastBuffer  = 8192
	wsClientSendBuffer = 4096
)

// Message is the structure sent over websocket
type Message struct {
	Type      MessageType `json:"type"`
	DeviceID  int         `json:"device_id,omitempty"`
	DeviceMAC string      `json:"device_mac,omitempty"`
	DeviceIP  string      `json:"device_ip,omitempty"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp int64       `json:"timestamp"`
}

// Client represents a connected websocket client
type Client struct {
	hub    *Hub
	conn   *websocket.Conn
	send   chan []byte
	userID int
}

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	mu             sync.RWMutex
	clients        map[*Client]bool
	broadcast      chan []byte
	register       chan *Client
	unregister     chan *Client
	allowedOrigins []string // Empty = same-origin only; "*" = allow all
}

// NewHub creates a new websocket hub
func NewHub() *Hub {
	return &Hub{
		clients:        make(map[*Client]bool),
		broadcast:      make(chan []byte, wsBroadcastBuffer),
		register:       make(chan *Client),
		unregister:     make(chan *Client),
		allowedOrigins: []string{}, // Default: same-origin only
	}
}

// enqueueDropOldest tries to enqueue msg onto ch without blocking.
// If the channel is full, it drops one oldest message and retries once.
// Returns true if msg was enqueued.
func enqueueDropOldest(ch chan []byte, msg []byte) bool {
	select {
	case ch <- msg:
		return true
	default:
		// Channel full: drop one oldest message and retry.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- msg:
			return true
		default:
			return false
		}
	}
}

// SetAllowedOrigins configures which origins can connect via WebSocket
// Empty slice = same-origin only (default, most secure)
// []string{"*"} = allow all origins (development mode)
// []string{"https://app.example.com"} = specific origins
func (h *Hub) SetAllowedOrigins(origins []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.allowedOrigins = origins
}

// checkOrigin validates the request origin against allowed origins
func (h *Hub) checkOrigin(r *http.Request) bool {
	h.mu.RLock()
	allowed := h.allowedOrigins
	h.mu.RUnlock()

	origin := r.Header.Get("Origin")

	// No origin header = same-origin request (browser doesn't send Origin for same-origin)
	if origin == "" {
		return true
	}

	// If no allowed origins configured, only allow same-origin
	if len(allowed) == 0 {
		// Compare origin to Host header
		originURL, err := url.Parse(origin)
		if err != nil {
			return false
		}
		// Same-origin: origin host matches request host
		return originURL.Host == r.Host
	}

	// Check for wildcard
	for _, a := range allowed {
		if a == "*" {
			return true
		}
	}

	// Check against explicit list
	for _, a := range allowed {
		if strings.EqualFold(origin, a) {
			return true
		}
		// Also match without trailing slash
		if strings.EqualFold(strings.TrimSuffix(origin, "/"), strings.TrimSuffix(a, "/")) {
			return true
		}
	}

	log.Printf("WebSocket: rejected origin %q (allowed: %v)", origin, allowed)
	return false
}

// Run starts the hub's main loop
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// Graceful shutdown: close all clients
			h.mu.Lock()
			for client := range h.clients {
				close(client.send)
				delete(h.clients, client)
			}
			h.mu.Unlock()
			log.Printf("WebSocket hub stopped")
			return

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("WebSocket client connected, total: %d", len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("WebSocket client disconnected, total: %d", len(h.clients))

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				// Non-blocking: if the client is backed up, drop an older message to make
				// room for the newest state (avoids stale online/offline in the UI).
				_ = enqueueDropOldest(client.send, message)
			}
			h.mu.RUnlock()
		}
	}
}

// Broadcast sends a message to all connected clients
func (h *Hub) Broadcast(msg Message) {
	msg.Timestamp = time.Now().Unix()
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("WebSocket marshal error: %v", err)
		return
	}

	if !enqueueDropOldest(h.broadcast, data) {
		log.Printf("WebSocket broadcast channel full")
	}
}

// BroadcastDeviceUpdate sends a device update to all clients
func (h *Hub) BroadcastDeviceUpdate(deviceID int, deviceIP string, data interface{}) {
	h.Broadcast(Message{
		Type:     MsgDeviceUpdate,
		DeviceID: deviceID,
		DeviceIP: deviceIP,
		Data:     data,
	})
}

// BroadcastStatsUpdate sends stats update for a device.
// IMPORTANT: device_id/device_mac are authoritative identifiers. device_ip is provided only as a fallback.
func (h *Hub) BroadcastStatsUpdate(deviceID int, deviceMAC, deviceIP string, data interface{}) {
	h.Broadcast(Message{
		Type:      MsgStatsUpdate,
		DeviceID:  deviceID,
		DeviceMAC: deviceMAC,
		DeviceIP:  deviceIP,
		Data:      data,
	})
}

// BroadcastJobUpdate sends job status update
func (h *Hub) BroadcastJobUpdate(jobID int, status string, data interface{}) {
	h.Broadcast(Message{
		Type: MsgJobUpdate,
		Data: map[string]interface{}{
			"job_id": jobID,
			"status": status,
			"data":   data,
		},
	})
}

// ClientCount returns the number of connected clients
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ServeWS handles websocket requests from clients
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, userID int) {
	// Create upgrader with our origin check
	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}

	client := &Client{
		hub:    h,
		conn:   conn,
		send:   make(chan []byte, wsClientSendBuffer),
		userID: userID,
	}

	h.register <- client

	go client.writePump()
	go client.readPump()
}

// readPump pumps messages from the websocket connection
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			break
		}

		// Handle ping messages
		var msg struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message, &msg) == nil && msg.Type == "ping" {
			pong, _ := json.Marshal(Message{Type: MsgPong, Timestamp: time.Now().Unix()})
			select {
			case c.send <- pong:
			default:
			}
		}
	}
}

// writePump pumps messages from the hub to the websocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Batch any queued messages
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
