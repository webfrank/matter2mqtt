package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

// ServerInfo is the unsolicited first frame python-matter-server sends on
// connect. It carries no message_id and no event field, which is how we
// distinguish it from everything else.
type ServerInfo struct {
	FabricID                  uint64 `json:"fabric_id"`
	CompressedFabricID        uint64 `json:"compressed_fabric_id"`
	SchemaVersion             int    `json:"schema_version"`
	MinSupportedSchemaVersion int    `json:"min_supported_schema_version"`
	SDKVersion                string `json:"sdk_version"`
	WifiCredentialsSet        bool   `json:"wifi_credentials_set"`
	ThreadCredentialsSet      bool   `json:"thread_credentials_set"`
	BluetoothEnabled          bool   `json:"bluetooth_enabled"`
}

// Node mirrors the node object returned by start_listening / get_nodes.
// Attributes is keyed by "<endpoint>/<cluster>/<attribute>", all numeric.
type Node struct {
	NodeID           uint64                     `json:"node_id"`
	DateCommissioned string                     `json:"date_commissioned"`
	LastInterview    string                     `json:"last_interview"`
	InterviewVersion int                        `json:"interview_version"`
	Available        bool                       `json:"available"`
	IsBridge         bool                       `json:"is_bridge"`
	Attributes       map[string]json.RawMessage `json:"attributes"`
}

// NodeEvent is the payload of the "node_event" event.
type NodeEvent struct {
	NodeID     uint64          `json:"node_id"`
	EndpointID int             `json:"endpoint_id"`
	ClusterID  uint32          `json:"cluster_id"`
	EventID    uint32          `json:"event_id"`
	Data       json.RawMessage `json:"data"`
}

// Event is a decoded server-pushed message.
type Event struct {
	Name string
	Data json.RawMessage
}

type wsCommand struct {
	MessageID string         `json:"message_id"`
	Command   string         `json:"command"`
	Args      map[string]any `json:"args,omitempty"`
}

type wsIncoming struct {
	MessageID string          `json:"message_id"`
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data"`
	Result    json.RawMessage `json:"result"`
	ErrorCode *int            `json:"error_code"`
	Details   string          `json:"details"`
}

// MatterClient is a single websocket session. It is not reused across
// reconnects: when the session dies, Events closes and the caller dials again.
type MatterClient struct {
	Info   ServerInfo
	Events chan Event

	conn    *websocket.Conn
	writeCh chan []byte
	nextID  atomic.Uint64

	mu      sync.Mutex
	pending map[string]chan *wsIncoming

	closeOnce sync.Once
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

// Dial opens a session and consumes the server-info frame before starting the
// read pump, so Info is populated by the time this returns.
func Dial(ctx context.Context, rawURL string) (*MatterClient, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial %s: %w (http %d)", rawURL, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("dial %s: %w", rawURL, err)
	}

	c := &MatterClient{
		Events:  make(chan Event, 256),
		conn:    conn,
		writeCh: make(chan []byte, 64),
		pending: make(map[string]chan *wsIncoming),
		done:    make(chan struct{}),
	}

	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	_, first, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read server info: %w", err)
	}
	if err := json.Unmarshal(first, &c.Info); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decode server info: %w", err)
	}

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	go c.readPump()
	go c.writePump()
	return c, nil
}

// Call issues a command and blocks until the matching response arrives.
func (c *MatterClient) Call(ctx context.Context, command string, args map[string]any) (json.RawMessage, error) {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	respCh := make(chan *wsIncoming, 1)

	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	payload, err := json.Marshal(wsCommand{MessageID: id, Command: command, Args: args})
	if err != nil {
		return nil, fmt.Errorf("encode command %s: %w", command, err)
	}

	select {
	case c.writeCh <- payload:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}

	select {
	case resp := <-respCh:
		if resp.ErrorCode != nil {
			return nil, fmt.Errorf("matter-server rejected %s: code %d: %s", command, *resp.ErrorCode, resp.Details)
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.Err()
	}
}

func (c *MatterClient) readPump() {
	defer c.closeWith(nil)
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			c.closeWith(fmt.Errorf("websocket read: %w", err))
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))

		var msg wsIncoming
		if err := json.Unmarshal(raw, &msg); err != nil {
			// A malformed frame is not fatal; skip it.
			continue
		}

		switch {
		case msg.MessageID != "":
			c.mu.Lock()
			ch, ok := c.pending[msg.MessageID]
			c.mu.Unlock()
			if ok {
				m := msg
				select {
				case ch <- &m:
				default:
				}
			}
		case msg.Event != "":
			select {
			case c.Events <- Event{Name: msg.Event, Data: msg.Data}:
			case <-c.done:
				return
			default:
				// Consumer is too slow. Dropping is better than stalling the
				// read pump and losing the connection entirely.
			}
		}
	}
}

func (c *MatterClient) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case payload := <-c.writeCh:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.closeWith(fmt.Errorf("websocket write: %w", err))
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				c.closeWith(fmt.Errorf("websocket ping: %w", err))
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *MatterClient) closeWith(err error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.errMu.Unlock()
		close(c.done)
		_ = c.conn.Close()
		close(c.Events)
	})
}

// Close terminates the session.
func (c *MatterClient) Close() {
	c.closeWith(fmt.Errorf("closed by caller"))
}

// Done is closed when the session ends for any reason.
func (c *MatterClient) Done() <-chan struct{} { return c.done }

// Err reports why the session ended.
func (c *MatterClient) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		return fmt.Errorf("session closed")
	}
	return c.err
}
