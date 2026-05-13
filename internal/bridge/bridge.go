// Package bridge connects the audio watcher to the HA WebSocket. One run of
// Run() is one full session: ensure entities exist, sync initial state, then
// forward changes both directions until ctx cancellation or a hard error.
//
// The caller (cmd/ha-volume) wraps Run() in a reconnect loop so a dropped
// WebSocket means "do the whole dance again," not "leak state."
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"ha-volume/internal/audio"
	"ha-volume/internal/config"
	"ha-volume/internal/haclient"
	"ha-volume/internal/logx"
)

// echoWindow is how long after WE push a value to HA (or vice-versa) we
// suppress the matching state_changed echo. State changes are otherwise
// instantaneous on a LAN, so 2s is plenty.
const echoWindow = 2 * time.Second

// Run blocks until ctx is done or an unrecoverable error. Returns nil on
// graceful shutdown via ctx.
func Run(ctx context.Context, cfg *config.Config, accessToken string, ac audio.Client) error {
	if cfg.HomeAssistantURL == "" {
		return errors.New("bridge: not configured")
	}

	logx.Infof("bridge: connecting to %s", cfg.HomeAssistantURL)
	ws, err := haclient.Dial(ctx, cfg.HomeAssistantURL, accessToken)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()

	// Resolve / create entities. EntityVolume / EntityMuted may be empty on
	// first run; we fill them in and persist.
	if cfg.EntityVolume == "" {
		cfg.EntityVolume = "input_number.windows_volume_" + hostSlug()
	}
	if cfg.EntityMuted == "" {
		cfg.EntityMuted = "input_boolean.windows_muted_" + hostSlug()
	}
	if err := ensureEntities(ctx, ws, cfg); err != nil {
		return fmt.Errorf("ensure entities: %w", err)
	}
	_ = config.Save(cfg)

	// Subscribe to state_changed for our two entities.
	events, unsub, err := ws.Subscribe(ctx, map[string]any{
		"type":       "subscribe_events",
		"event_type": "state_changed",
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer unsub()

	audioEvents := ac.Watch(ctx)

	// Push initial state to HA so the slider in HA reflects the real
	// machine state right after a fresh connect.
	if v, err := ac.Volume(); err == nil {
		pushVolume(ctx, ws, cfg.EntityVolume, v)
	}
	if m, err := ac.Muted(); err == nil {
		pushMuted(ctx, ws, cfg.EntityMuted, m)
	}

	b := &state{
		ws:           ws,
		cfg:          cfg,
		audio:        ac,
		lastPushedVol: -1,
		lastPushedMut: -1,
	}

	logx.Infof("bridge: ready (volume=%s muted=%s)", cfg.EntityVolume, cfg.EntityMuted)

	for {
		select {
		case <-ctx.Done():
			return nil
		case raw, ok := <-events:
			if !ok {
				return errors.New("ws closed")
			}
			b.handleHAEvent(ctx, raw)
		case st, ok := <-audioEvents:
			if !ok {
				return errors.New("audio closed")
			}
			b.handleAudioChange(ctx, st)
		}
	}
}

// state holds per-session mutable bridge state.
type state struct {
	ws    *haclient.WS
	cfg   *config.Config
	audio audio.Client

	// lastPushed* + their stamps form the echo-suppression window. We
	// remember the value WE most recently sent in each direction.
	lastPushedVol  int
	lastPushedVolT time.Time
	lastPushedMut  int // -1 unset, 0 false, 1 true (avoids extra bool field)
	lastPushedMutT time.Time

	lastReceivedVol  int
	lastReceivedVolT time.Time
	lastReceivedMut  int
	lastReceivedMutT time.Time
}

func (b *state) handleAudioChange(ctx context.Context, st audio.State) {
	// If this matches a value HA just told us, it's the result of our own
	// SetVolume — don't push it back, suppress the echo.
	if b.lastReceivedVol == st.Volume && time.Since(b.lastReceivedVolT) < echoWindow {
		// fall through to mute logic
	} else {
		if err := pushVolume(ctx, b.ws, b.cfg.EntityVolume, st.Volume); err != nil {
			logx.Warnf("push volume: %v", err)
		} else {
			b.lastPushedVol = st.Volume
			b.lastPushedVolT = time.Now()
		}
	}

	mInt := boolToInt(st.Muted)
	if b.lastReceivedMut == mInt && time.Since(b.lastReceivedMutT) < echoWindow {
		return
	}
	if err := pushMuted(ctx, b.ws, b.cfg.EntityMuted, st.Muted); err != nil {
		logx.Warnf("push mute: %v", err)
	} else {
		b.lastPushedMut = mInt
		b.lastPushedMutT = time.Now()
	}
}

func (b *state) handleHAEvent(ctx context.Context, raw json.RawMessage) {
	var ev struct {
		Event struct {
			Data haclient.StateChangedData `json:"data"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}
	d := ev.Event.Data
	if d.NewState == nil {
		return // entity removed
	}

	switch d.EntityID {
	case b.cfg.EntityVolume:
		v, err := strconv.Atoi(strings.SplitN(d.NewState.State, ".", 2)[0])
		if err != nil {
			// HA may report "47.0" as a float string; tolerate.
			if f, err2 := strconv.ParseFloat(d.NewState.State, 64); err2 == nil {
				v = int(f + 0.5)
			} else {
				return
			}
		}
		// Echo from our own push?
		if b.lastPushedVol == v && time.Since(b.lastPushedVolT) < echoWindow {
			return
		}
		b.lastReceivedVol = v
		b.lastReceivedVolT = time.Now()
		if err := b.audio.SetVolume(v); err != nil {
			logx.Warnf("set volume %d: %v", v, err)
		}

	case b.cfg.EntityMuted:
		m := d.NewState.State == "on"
		mInt := boolToInt(m)
		if b.lastPushedMut == mInt && time.Since(b.lastPushedMutT) < echoWindow {
			return
		}
		b.lastReceivedMut = mInt
		b.lastReceivedMutT = time.Now()
		if err := b.audio.SetMuted(m); err != nil {
			logx.Warnf("set muted %v: %v", m, err)
		}
	}
}

func pushVolume(ctx context.Context, ws *haclient.WS, entityID string, v int) error {
	return ws.CallService(ctx, "input_number", "set_value", nil, map[string]any{
		"entity_id": entityID,
		"value":     v,
	})
}

func pushMuted(ctx context.Context, ws *haclient.WS, entityID string, m bool) error {
	service := "turn_off"
	if m {
		service = "turn_on"
	}
	return ws.CallService(ctx, "input_boolean", service, nil, map[string]any{
		"entity_id": entityID,
	})
}

// ensureEntities makes sure our two helpers exist. We check via get_state
// first (cheap), then list, then create. The create call slug-matches
// EntityVolume — HA derives entity_id from `name` so the slug has to match.
func ensureEntities(ctx context.Context, ws *haclient.WS, cfg *config.Config) error {
	if err := ensureInputNumber(ctx, ws, cfg.EntityVolume, cfg.Step); err != nil {
		return err
	}
	return ensureInputBoolean(ctx, ws, cfg.EntityMuted)
}

func ensureInputNumber(ctx context.Context, ws *haclient.WS, entityID string, step int) error {
	s, err := ws.GetState(ctx, entityID)
	if err != nil {
		return err
	}
	if s != nil {
		return nil
	}
	// Need to create. HA's slugify lowercases and substitutes non-alphanum
	// with underscores, so passing the bare slug as `name` reproduces the
	// expected entity_id.
	name := strings.TrimPrefix(entityID, "input_number.")
	if step <= 0 {
		step = 1
	}
	_, err = ws.Call(ctx, map[string]any{
		"type":     "input_number/create",
		"name":     name,
		"min":      0,
		"max":      100,
		"step":     step,
		"mode":     "slider",
		"icon":     "mdi:volume-high",
		"initial":  50,
	})
	if err != nil {
		return fmt.Errorf("input_number/create: %w", err)
	}
	logx.Infof("created entity %s", entityID)
	return nil
}

func ensureInputBoolean(ctx context.Context, ws *haclient.WS, entityID string) error {
	s, err := ws.GetState(ctx, entityID)
	if err != nil {
		return err
	}
	if s != nil {
		return nil
	}
	name := strings.TrimPrefix(entityID, "input_boolean.")
	_, err = ws.Call(ctx, map[string]any{
		"type": "input_boolean/create",
		"name": name,
		"icon": "mdi:volume-mute",
	})
	if err != nil {
		return fmt.Errorf("input_boolean/create: %w", err)
	}
	logx.Infof("created entity %s", entityID)
	return nil
}

// hostSlug returns a slugified hostname suitable for an HA entity_id suffix.
func hostSlug() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "pc"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && b.String()[b.Len()-1] != '_' {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		out = "pc"
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
