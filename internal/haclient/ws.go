package haclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WS is a minimal Home Assistant WebSocket client. It handles the auth
// handshake, sequential message IDs, request/response correlation, and
// subscriptions. Reconnect logic lives in the caller (bridge) so it can
// coordinate with state recovery.
type WS struct {
	conn *websocket.Conn

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan json.RawMessage // request → response
	subs    map[uint64]chan json.RawMessage // subscription → event stream

	closed atomic.Bool
}

// Event represents a state_changed event payload. Other event types reuse
// the same struct; consumers parse Data themselves.
type Event struct {
	EventType string          `json:"event_type"`
	Data      json.RawMessage `json:"data"`
	TimeFired time.Time       `json:"time_fired"`
	Origin    string          `json:"origin"`
}

// StateChangedData is what HA puts inside Event.Data for state_changed.
type StateChangedData struct {
	EntityID string `json:"entity_id"`
	OldState *State `json:"old_state"`
	NewState *State `json:"new_state"`
}

// State is HA's standard entity state envelope.
type State struct {
	EntityID    string                 `json:"entity_id"`
	State       string                 `json:"state"`
	Attributes  map[string]any         `json:"attributes,omitempty"`
	LastChanged time.Time              `json:"last_changed,omitempty"`
	LastUpdated time.Time              `json:"last_updated,omitempty"`
	Context     map[string]any         `json:"context,omitempty"`
}

// Dial connects to ws[s]://<hass>/api/websocket and authenticates with the
// given access token.
func Dial(ctx context.Context, hassURL, accessToken string) (*WS, error) {
	u, err := url.Parse(hassURL)
	if err != nil {
		return nil, fmt.Errorf("parse hass url: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/websocket"

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	conn, _, err := dialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	// auth_required → auth → auth_ok / auth_invalid.
	var hello struct {
		Type    string `json:"type"`
		Message string `json:"message,omitempty"`
	}
	if err := conn.ReadJSON(&hello); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws auth_required: %w", err)
	}
	if hello.Type != "auth_required" {
		conn.Close()
		return nil, fmt.Errorf("unexpected first frame: %s", hello.Type)
	}
	if err := conn.WriteJSON(map[string]any{
		"type":         "auth",
		"access_token": accessToken,
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws auth send: %w", err)
	}
	var ack struct {
		Type    string `json:"type"`
		Message string `json:"message,omitempty"`
	}
	if err := conn.ReadJSON(&ack); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws auth ack: %w", err)
	}
	if ack.Type != "auth_ok" {
		conn.Close()
		return nil, fmt.Errorf("ws auth rejected: %s", ack.Message)
	}

	w := &WS{
		conn:    conn,
		pending: make(map[uint64]chan json.RawMessage),
		subs:    make(map[uint64]chan json.RawMessage),
	}
	w.nextID.Store(0) // HA expects IDs starting at 1; we Add(1) before use.
	go w.readLoop()
	return w, nil
}

// IsTokenExpired distinguishes a 401-style WS rejection so the caller knows
// to refresh and reconnect rather than bail.
func IsTokenExpired(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ws auth rejected")
}

func (w *WS) Close() error {
	if w.closed.Swap(true) {
		return nil
	}
	// Best-effort: close everything we know about so blocked goroutines exit.
	_ = w.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	err := w.conn.Close()
	w.mu.Lock()
	for id, ch := range w.pending {
		close(ch)
		delete(w.pending, id)
	}
	for id, ch := range w.subs {
		close(ch)
		delete(w.subs, id)
	}
	w.mu.Unlock()
	return err
}

// readLoop demuxes incoming frames into pending request channels or
// subscription channels.
func (w *WS) readLoop() {
	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			w.Close()
			return
		}
		// Peek minimally.
		var head struct {
			ID    uint64 `json:"id"`
			Type  string `json:"type"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue
		}
		w.mu.Lock()
		switch head.Type {
		case "result":
			if ch, ok := w.pending[head.ID]; ok {
				ch <- data
				delete(w.pending, head.ID)
			}
		case "event":
			if ch, ok := w.subs[head.ID]; ok {
				select {
				case ch <- data:
				default:
					// Slow consumer; drop. State subscriptions are
					// idempotent so dropping one event isn't fatal.
				}
			}
		case "pong":
			// nothing
		}
		w.mu.Unlock()
	}
}

// send writes a JSON message with a fresh ID and returns the result channel.
func (w *WS) send(msg map[string]any) (uint64, <-chan json.RawMessage, error) {
	id := w.nextID.Add(1)
	msg["id"] = id
	ch := make(chan json.RawMessage, 1)
	w.mu.Lock()
	w.pending[id] = ch
	w.mu.Unlock()
	w.writeMu.Lock()
	err := w.conn.WriteJSON(msg)
	w.writeMu.Unlock()
	if err != nil {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return 0, nil, err
	}
	return id, ch, nil
}

// Call performs a single request/response. The map should contain at least
// {"type": "..."}; id is added.
func (w *WS) Call(ctx context.Context, msg map[string]any) (json.RawMessage, error) {
	_, ch, err := w.send(msg)
	if err != nil {
		return nil, err
	}
	select {
	case raw, ok := <-ch:
		if !ok {
			return nil, errors.New("ws: connection closed")
		}
		var r struct {
			Success bool            `json:"success"`
			Error   *struct{ Message string `json:"message"` } `json:"error"`
			Result  json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if !r.Success {
			if r.Error != nil {
				return nil, fmt.Errorf("ws: %s", r.Error.Message)
			}
			return nil, errors.New("ws: call failed")
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Subscribe registers a subscription, returns an event channel + an
// unsubscribe func. The channel closes when the WS closes or on unsubscribe.
func (w *WS) Subscribe(ctx context.Context, msg map[string]any) (<-chan json.RawMessage, func(), error) {
	// Reserve the subscription slot BEFORE sending so we don't race with
	// the server's first event — HA may push events before we've finished
	// processing the success ack frame.
	id := w.nextID.Add(1)
	msg["id"] = id
	events := make(chan json.RawMessage, 16)
	resultCh := make(chan json.RawMessage, 1)
	w.mu.Lock()
	w.pending[id] = resultCh
	w.subs[id] = events
	w.mu.Unlock()

	w.writeMu.Lock()
	werr := w.conn.WriteJSON(msg)
	w.writeMu.Unlock()
	if werr != nil {
		w.mu.Lock()
		delete(w.pending, id)
		delete(w.subs, id)
		w.mu.Unlock()
		return nil, nil, werr
	}

	// Wait for the success ack.
	select {
	case raw, ok := <-resultCh:
		if !ok {
			return nil, nil, errors.New("ws: connection closed")
		}
		var r struct {
			Success bool `json:"success"`
			Error   *struct{ Message string `json:"message"` } `json:"error"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			w.mu.Lock()
			delete(w.subs, id)
			w.mu.Unlock()
			return nil, nil, err
		}
		if !r.Success {
			w.mu.Lock()
			delete(w.subs, id)
			w.mu.Unlock()
			if r.Error != nil {
				return nil, nil, fmt.Errorf("subscribe: %s", r.Error.Message)
			}
			return nil, nil, errors.New("subscribe failed")
		}
	case <-ctx.Done():
		w.mu.Lock()
		delete(w.subs, id)
		w.mu.Unlock()
		return nil, nil, ctx.Err()
	}
	unsub := func() {
		w.mu.Lock()
		if ch, ok := w.subs[id]; ok {
			delete(w.subs, id)
			close(ch)
		}
		w.mu.Unlock()
		_, _, _ = w.send(map[string]any{
			"type":         "unsubscribe_events",
			"subscription": id,
		})
	}
	return events, unsub, nil
}

// CallService is sugar for {"type":"call_service", ...}.
func (w *WS) CallService(ctx context.Context, domain, service string, target map[string]any, data map[string]any) error {
	msg := map[string]any{
		"type":            "call_service",
		"domain":          domain,
		"service":         service,
		"service_data":    data,
	}
	if target != nil {
		msg["target"] = target
	}
	_, err := w.Call(ctx, msg)
	return err
}

// GetState is sugar for {"type":"get_states"} filtered to a single entity.
// Returns nil, nil if the entity does not exist.
func (w *WS) GetState(ctx context.Context, entityID string) (*State, error) {
	raw, err := w.Call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		return nil, err
	}
	var states []State
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, err
	}
	for i := range states {
		if states[i].EntityID == entityID {
			return &states[i], nil
		}
	}
	return nil, nil
}

