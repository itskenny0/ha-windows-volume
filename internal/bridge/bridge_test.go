package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ha-volume/internal/audio"
	"ha-volume/internal/config"
)

// fakeHA implements just enough of HA's WS protocol for the bridge to walk
// through ensureEntities + state sync, and lets the test inject HA-side state
// changes via Push.
type fakeHA struct {
	t *testing.T

	mu     sync.Mutex
	conn   *websocket.Conn
	states map[string]string // entity_id → state
	subID  uint64

	// recorded service calls
	calls []serviceCall
}

type serviceCall struct {
	Domain  string
	Service string
	Data    map[string]any
}

func newFakeHA(t *testing.T) (string, *fakeHA) {
	f := &fakeHA{
		t:      t,
		states: map[string]string{},
	}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/websocket" {
			http.NotFound(w, r)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		f.mu.Lock()
		f.conn = c
		f.mu.Unlock()
		_ = c.WriteJSON(map[string]any{"type": "auth_required"})
		var auth map[string]any
		if err := c.ReadJSON(&auth); err != nil {
			return
		}
		_ = c.WriteJSON(map[string]any{"type": "auth_ok"})
		f.loop()
	}))
	t.Cleanup(srv.Close)
	return srv.URL, f
}

func (f *fakeHA) loop() {
	for {
		_, data, err := f.conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		id, _ := m["id"].(float64)
		switch m["type"] {
		case "get_states":
			f.mu.Lock()
			var out []map[string]any
			for k, v := range f.states {
				out = append(out, map[string]any{"entity_id": k, "state": v})
			}
			f.mu.Unlock()
			b, _ := json.Marshal(out)
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true, "result": json.RawMessage(b),
			})
		case "input_number/create":
			name, _ := m["name"].(string)
			f.mu.Lock()
			f.states["input_number."+name] = "50"
			f.mu.Unlock()
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true,
			})
		case "input_boolean/create":
			name, _ := m["name"].(string)
			f.mu.Lock()
			f.states["input_boolean."+name] = "off"
			f.mu.Unlock()
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true,
			})
		case "subscribe_events":
			f.mu.Lock()
			f.subID = uint64(id)
			f.mu.Unlock()
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true,
			})
		case "unsubscribe_events":
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true,
			})
		case "call_service":
			data, _ := m["service_data"].(map[string]any)
			f.mu.Lock()
			f.calls = append(f.calls, serviceCall{
				Domain:  m["domain"].(string),
				Service: m["service"].(string),
				Data:    data,
			})
			// Apply the call to our state model so subsequent get_state
			// reflects reality.
			eid, _ := data["entity_id"].(string)
			switch m["domain"] {
			case "input_number":
				if v, ok := data["value"].(float64); ok {
					f.states[eid] = trimNumber(v)
				}
			case "input_boolean":
				if m["service"] == "turn_on" {
					f.states[eid] = "on"
				} else {
					f.states[eid] = "off"
				}
			}
			f.mu.Unlock()
			_ = f.conn.WriteJSON(map[string]any{
				"id": id, "type": "result", "success": true,
			})
		}
	}
}

// Push delivers a state_changed event for entity to the bridge.
func (f *fakeHA) Push(entity, state string) {
	f.mu.Lock()
	id := f.subID
	f.states[entity] = state
	c := f.conn
	f.mu.Unlock()
	if c == nil || id == 0 {
		return
	}
	_ = c.WriteJSON(map[string]any{
		"id":   id,
		"type": "event",
		"event": map[string]any{
			"event_type": "state_changed",
			"data": map[string]any{
				"entity_id": entity,
				"new_state": map[string]any{"entity_id": entity, "state": state},
			},
		},
	})
}

func (f *fakeHA) Calls() []serviceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]serviceCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func trimNumber(v float64) string {
	// HA reports numbers like "50.0"; matching that here helps the
	// bridge's float-parse path get exercised too.
	if v == float64(int(v)) {
		return strings_TrimRight(formatFloat(v))
	}
	return formatFloat(v)
}

// Use a tiny formatter to avoid pulling strconv just for this.
func formatFloat(v float64) string {
	// Always one decimal, matching HA conventions for input_number.
	whole := int(v)
	dec := int((v - float64(whole)) * 10)
	if dec < 0 {
		dec = -dec
	}
	return itoa(whole) + "." + string(rune('0'+dec))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{'0' + byte(i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func strings_TrimRight(s string) string { return strings.TrimRight(s, "0.") + suffix(s) }
func suffix(s string) string {
	if strings.HasSuffix(s, ".0") {
		return ".0"
	}
	return ""
}

// stubAudio is an in-memory Client used by the bridge under test.
type stubAudio struct {
	vol   atomic.Int32
	mut   atomic.Bool
	emit  chan audio.State
	close chan struct{}
}

func newStubAudio() *stubAudio {
	s := &stubAudio{emit: make(chan audio.State, 8), close: make(chan struct{})}
	s.vol.Store(20)
	return s
}

func (s *stubAudio) Volume() (int, error)   { return int(s.vol.Load()), nil }
func (s *stubAudio) SetVolume(p int) error  { s.vol.Store(int32(p)); s.emit <- audio.State{Volume: p, Muted: s.mut.Load()}; return nil }
func (s *stubAudio) Muted() (bool, error)   { return s.mut.Load(), nil }
func (s *stubAudio) SetMuted(b bool) error  { s.mut.Store(b); s.emit <- audio.State{Volume: int(s.vol.Load()), Muted: b}; return nil }
func (s *stubAudio) DeviceName() string     { return "stub" }
func (s *stubAudio) Close() error           { close(s.close); return nil }
func (s *stubAudio) Watch(ctx context.Context) <-chan audio.State {
	out := make(chan audio.State, 8)
	// Emit initial state.
	out <- audio.State{Volume: int(s.vol.Load()), Muted: s.mut.Load()}
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.close:
				return
			case st := <-s.emit:
				select {
				case out <- st:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// LocalChange simulates an external Windows-side volume change (e.g. user
// pressed the volume key).
func (s *stubAudio) LocalChange(v int, m bool) {
	s.vol.Store(int32(v))
	s.mut.Store(m)
	s.emit <- audio.State{Volume: v, Muted: m}
}

func TestBridge_LocalChangeIsForwardedToHA(t *testing.T) {
	url, fake := newFakeHA(t)
	ac := newStubAudio()
	cfg := &config.Config{
		HomeAssistantURL: url,
		Step:             1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "token", ac) }()

	// Give Run time to subscribe + push initial state.
	time.Sleep(400 * time.Millisecond)

	// Simulate a local volume change.
	ac.LocalChange(73, false)

	// Wait for the HA-side service call.
	if !waitFor(t, 2*time.Second, func() bool {
		for _, c := range fake.Calls() {
			if c.Domain == "input_number" && c.Service == "set_value" {
				if v, ok := c.Data["value"].(float64); ok && int(v) == 73 {
					return true
				}
			}
		}
		return false
	}) {
		t.Fatalf("HA never saw set_value(73). Calls: %+v", fake.Calls())
	}

	cancel()
	<-done
}

func TestBridge_RemoteChangeUpdatesLocal(t *testing.T) {
	url, fake := newFakeHA(t)
	ac := newStubAudio()
	cfg := &config.Config{HomeAssistantURL: url, Step: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "token", ac) }()

	time.Sleep(400 * time.Millisecond)
	fake.Push(cfg.EntityVolume, "37")

	if !waitFor(t, 2*time.Second, func() bool {
		v, _ := ac.Volume()
		return v == 37
	}) {
		v, _ := ac.Volume()
		t.Fatalf("local volume did not update from HA: got %d want 37", v)
	}

	cancel()
	<-done
}

func TestBridge_EchoIsSuppressed(t *testing.T) {
	url, fake := newFakeHA(t)
	ac := newStubAudio()
	cfg := &config.Config{HomeAssistantURL: url, Step: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, "token", ac) }()

	time.Sleep(400 * time.Millisecond)

	// Local change at 60 → bridge pushes to HA. HA echoes via state_changed.
	// We want exactly ONE outgoing call (the original push), not two.
	startCalls := len(fake.Calls())
	ac.LocalChange(60, false)
	time.Sleep(300 * time.Millisecond)
	// HA confirms by pushing the same value back to the bridge.
	fake.Push(cfg.EntityVolume, "60")
	time.Sleep(500 * time.Millisecond)

	setCalls := 0
	for _, c := range fake.Calls()[startCalls:] {
		if c.Domain == "input_number" && c.Service == "set_value" {
			setCalls++
		}
	}
	if setCalls != 1 {
		t.Fatalf("expected exactly one set_value after local change, got %d", setCalls)
	}

	cancel()
	<-done
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
