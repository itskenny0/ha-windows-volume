// Package tray owns the systray icon and menu. It does NOT own the audio /
// HA bridge — it just talks to it via the Controller interface so we can
// swap the impl in tests.
//
// fyne.io/systray requires Run() to be called on the main goroutine on macOS;
// on Windows it's not strictly required but we keep the convention.
package tray

import (
	"fmt"
	"image/color"
	"sync"

	"fyne.io/systray"

	"ha-volume/internal/logx"
)

// Controller is what the tray needs from the rest of the app: status text,
// callbacks for menu items, and a way to be notified of state changes.
type Controller interface {
	Status() Status
	OpenSettings()
	OpenHomeAssistant()
	Reconnect()
	Quit()
}

// Status is a snapshot of what the tray should show.
type Status struct {
	Connected   bool
	Volume      int
	Muted       bool
	DeviceName  string
	StatusText  string // one-line description for the menu header
	HassURL     string
}

// Tray is the live tray instance. Only one per process.
type Tray struct {
	c Controller

	mu       sync.Mutex
	mHeader  *systray.MenuItem
	mDevice  *systray.MenuItem
	mVolume  *systray.MenuItem
	mURL     *systray.MenuItem
}

// New creates the tray instance bound to controller c. Call Run to start it
// (blocking) and Quit to stop it. The tray uses systray's internal main loop,
// so the caller should treat Run like systray.Run.
func New(c Controller) *Tray { return &Tray{c: c} }

// Run starts the systray. Blocks until Quit() is called.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Quit asks systray to stop.
func (t *Tray) Quit() { systray.Quit() }

// Refresh re-reads status from the controller and updates the menu. Safe to
// call from any goroutine after systray.Run has reached onReady.
func (t *Tray) Refresh() {
	st := t.c.Status()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.mHeader != nil {
		t.mHeader.SetTitle(st.StatusText)
	}
	if t.mDevice != nil {
		t.mDevice.SetTitle("Device: " + emptyDefault(st.DeviceName, "(unknown)"))
	}
	if t.mVolume != nil {
		t.mVolume.SetTitle(fmt.Sprintf("Volume: %d%%%s", st.Volume, muteSuffix(st.Muted)))
	}
	if t.mURL != nil {
		if st.HassURL == "" {
			t.mURL.SetTitle("Open Home Assistant (not configured)")
			t.mURL.Disable()
		} else {
			t.mURL.SetTitle("Open Home Assistant")
			t.mURL.Enable()
		}
	}
	systray.SetTooltip(fmt.Sprintf("HA Volume — %s", st.StatusText))
	systray.SetIcon(buildIcon(iconColor(st.Connected), !st.Connected))
}

func (t *Tray) onReady() {
	st := t.c.Status()
	systray.SetIcon(buildIcon(iconColor(st.Connected), !st.Connected))
	systray.SetTitle("")
	systray.SetTooltip("HA Volume")

	t.mu.Lock()
	t.mHeader = systray.AddMenuItem(st.StatusText, "Current status")
	t.mHeader.Disable()
	t.mDevice = systray.AddMenuItem("Device: …", "Active audio output")
	t.mDevice.Disable()
	t.mVolume = systray.AddMenuItem("Volume: …", "Current system volume")
	t.mVolume.Disable()
	systray.AddSeparator()

	mSettings := systray.AddMenuItem("Settings…", "Open the settings page in your browser")
	t.mURL = systray.AddMenuItem("Open Home Assistant", "Open the configured HA instance")
	mReconnect := systray.AddMenuItem("Reconnect", "Drop and re-establish the HA connection")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit HA Volume")
	t.mu.Unlock()

	// Refresh now that menu items exist.
	t.Refresh()

	go func() {
		for {
			select {
			case <-mSettings.ClickedCh:
				t.c.OpenSettings()
			case <-t.mURL.ClickedCh:
				t.c.OpenHomeAssistant()
			case <-mReconnect.ClickedCh:
				t.c.Reconnect()
			case <-mQuit.ClickedCh:
				logx.Infof("tray: quit selected")
				t.c.Quit()
				return
			}
		}
	}()
}

func (t *Tray) onExit() {
	logx.Infof("tray: systray exiting")
}

func iconColor(connected bool) color.RGBA {
	if connected {
		// HA blue.
		return color.RGBA{R: 0x03, G: 0xa9, B: 0xf4, A: 0xff}
	}
	// Dim grey for "not connected".
	return color.RGBA{R: 0x9e, G: 0x9e, B: 0x9e, A: 0xff}
}

func muteSuffix(m bool) string {
	if m {
		return " (muted)"
	}
	return ""
}

func emptyDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
