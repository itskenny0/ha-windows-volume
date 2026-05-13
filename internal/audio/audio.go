// Package audio bridges the Windows Core Audio API to the rest of the app.
//
// We expose a tiny surface:
//
//	c := audio.Open()           // initialises COM + grabs default render endpoint
//	defer c.Close()
//	v, err := c.Volume()        // 0..100
//	err = c.SetVolume(50)
//	m, err := c.Muted()
//	err = c.SetMuted(true)
//	ch := c.Watch(ctx)          // emits State whenever volume/mute change (polled)
//
// We deliberately don't expose the underlying go-wca interfaces — keeping the
// abstraction skinny makes the Linux stub possible and the bridge testable.
package audio

import "context"

// State is what the bridge consumes: volume percent + mute flag.
type State struct {
	Volume int  // 0..100
	Muted  bool
}

// Client is the platform-specific implementation. The real one lives in
// audio_windows.go; audio_other.go provides a stub for cross-platform builds.
type Client interface {
	Volume() (int, error)
	SetVolume(percent int) error
	Muted() (bool, error)
	SetMuted(b bool) error
	DeviceName() string

	// Watch returns a channel that yields the current state whenever it
	// changes. It is closed when ctx is cancelled or the client is closed.
	// The first emission is the initial state.
	Watch(ctx context.Context) <-chan State

	Close() error
}

// Open returns a connected Client or an error. Platform-specific.
func Open() (Client, error) { return openImpl() }
