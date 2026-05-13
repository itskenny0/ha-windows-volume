package haclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeHA stands in for Home Assistant's /api/websocket endpoint.
type fakeHA struct {
	t            *testing.T
	wantToken    string
	upgrader     websocket.Upgrader
	onceAuth     sync.Once
	receivedAuth bool
}

func (f *fakeHA) handle(w http.ResponseWriter, r *http.Request) {
	conn, err := f.upgrader.Upgrade(w, r, nil)
	if err != nil {
		f.t.Fatalf("upgrade: %v", err)
	}
	defer conn.Close()
	_ = conn.WriteJSON(map[string]any{"type": "auth_required", "ha_version": "test"})

	var auth struct {
		Type        string `json:"type"`
		AccessToken string `json:"access_token"`
	}
	if err := conn.ReadJSON(&auth); err != nil {
		return
	}
	f.onceAuth.Do(func() { f.receivedAuth = true })
	if auth.AccessToken != f.wantToken {
		_ = conn.WriteJSON(map[string]any{"type": "auth_invalid", "message": "wrong token"})
		return
	}
	_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		id, _ := msg["id"].(float64)
		switch msg["type"] {
		case "get_states":
			states := []map[string]any{
				{"entity_id": "sensor.foo", "state": "42"},
			}
			b, _ := json.Marshal(states)
			_ = conn.WriteJSON(map[string]any{
				"id":      id,
				"type":    "result",
				"success": true,
				"result":  json.RawMessage(b),
			})
		case "subscribe_events":
			_ = conn.WriteJSON(map[string]any{
				"id":      id,
				"type":    "result",
				"success": true,
			})
			// Push one event then idle.
			_ = conn.WriteJSON(map[string]any{
				"id":   id,
				"type": "event",
				"event": map[string]any{
					"event_type": "state_changed",
					"data": map[string]any{
						"entity_id": "sensor.foo",
						"new_state": map[string]any{"entity_id": "sensor.foo", "state": "99"},
					},
				},
			})
		case "call_service":
			_ = conn.WriteJSON(map[string]any{
				"id":      id,
				"type":    "result",
				"success": true,
			})
		default:
			_ = conn.WriteJSON(map[string]any{
				"id":      id,
				"type":    "result",
				"success": false,
				"error":   map[string]any{"message": "unsupported"},
			})
		}
	}
}

func newFakeHA(t *testing.T, token string) (string, *fakeHA) {
	f := &fakeHA{t: t, wantToken: token, upgrader: websocket.Upgrader{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/websocket" {
			http.NotFound(w, r)
			return
		}
		f.handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.Replace(srv.URL, "http://", "http://", 1), f
}

func TestWS_AuthAndGetState(t *testing.T) {
	url, fake := newFakeHA(t, "good")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, err := Dial(ctx, url, "good")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer ws.Close()
	if !fake.receivedAuth {
		t.Fatal("server didn't see auth message")
	}
	st, err := ws.GetState(ctx, "sensor.foo")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if st == nil || st.State != "42" {
		t.Fatalf("got %+v", st)
	}
	// Missing entity returns (nil, nil).
	st, err = ws.GetState(ctx, "sensor.bar")
	if err != nil || st != nil {
		t.Fatalf("expected (nil,nil), got (%v,%v)", st, err)
	}
}

func TestWS_BadToken(t *testing.T) {
	url, _ := newFakeHA(t, "good")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Dial(ctx, url, "bad")
	if err == nil {
		t.Fatal("expected auth rejection")
	}
	if !IsTokenExpired(err) {
		t.Fatalf("IsTokenExpired should match: %v", err)
	}
}

func TestWS_Subscribe(t *testing.T) {
	url, _ := newFakeHA(t, "good")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, err := Dial(ctx, url, "good")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer ws.Close()
	ch, unsub, err := ws.Subscribe(ctx, map[string]any{
		"type":       "subscribe_events",
		"event_type": "state_changed",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()
	select {
	case raw := <-ch:
		if !strings.Contains(string(raw), `"entity_id":"sensor.foo"`) {
			t.Fatalf("unexpected event: %s", raw)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
}
