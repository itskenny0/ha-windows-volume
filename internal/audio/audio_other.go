//go:build !windows

package audio

import (
	"context"
	"sync/atomic"
	"time"
)

// stubClient lets us compile + run rudimentary tests on Linux. It models
// volume as an in-memory integer with no real audio side-effects.
type stubClient struct {
	vol   atomic.Int32
	muted atomic.Bool
}

func openImpl() (Client, error) {
	c := &stubClient{}
	c.vol.Store(50)
	return c, nil
}

func (c *stubClient) Volume() (int, error)        { return int(c.vol.Load()), nil }
func (c *stubClient) SetVolume(p int) error       { c.vol.Store(int32(clamp(p))); return nil }
func (c *stubClient) Muted() (bool, error)        { return c.muted.Load(), nil }
func (c *stubClient) SetMuted(b bool) error       { c.muted.Store(b); return nil }
func (c *stubClient) DeviceName() string          { return "stub" }
func (c *stubClient) Close() error                { return nil }
func (c *stubClient) Watch(ctx context.Context) <-chan State {
	out := make(chan State, 4)
	go func() {
		defer close(out)
		var prev State
		first := true
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			cur := State{Volume: int(c.vol.Load()), Muted: c.muted.Load()}
			if first || cur != prev {
				select {
				case out <- cur:
				case <-ctx.Done():
					return
				}
				prev = cur
				first = false
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func clamp(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

