// ha-volume — Home Assistant ↔ Windows master-volume bridge.
//
// Layout:
//
//	main.go        wires everything: tray, settings, bridge, OAuth, audio.
//	The actual logic lives in internal/...
//
// The app's main goroutine runs the tray (systray requires the main thread
// on macOS; on Windows it's fine either way). Everything else is goroutines
// coordinated by an *App which the tray and settings page both poke.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ha-volume/internal/audio"
	"ha-volume/internal/bridge"
	"ha-volume/internal/config"
	"ha-volume/internal/haclient"
	"ha-volume/internal/logx"
	"ha-volume/internal/settings"
	"ha-volume/internal/startup"
	"ha-volume/internal/tray"
)

func main() {
	openSettings := flag.Bool("settings", false, "open the settings page in your browser and exit")
	flag.Parse()

	cfgDir, _ := config.Dir()
	if err := logx.Init(cfgDir); err != nil {
		fmt.Fprintln(os.Stderr, "log init:", err)
	}
	logx.Infof("ha-volume starting (pid=%d)", os.Getpid())

	cfg, err := config.Load()
	if err != nil {
		logx.Errorf("load config: %v", err)
		cfg = &config.Config{}
		cfg.Defaults()
	}

	ac, err := audio.Open()
	if err != nil {
		logx.Errorf("audio.Open: %v — running without local volume control", err)
	}

	app := &App{
		cfg:   cfg,
		audio: ac,
	}
	app.statusMsg.Store("Starting…")

	// Settings server.
	ss, err := settings.Start(app)
	if err != nil {
		logx.Errorf("settings server: %v", err)
		os.Exit(1)
	}
	app.settings = ss
	defer ss.Stop()

	if *openSettings {
		// Spawned solely to open the settings page (e.g. from a Start Menu
		// shortcut). Open and exit — another instance is the real worker.
		_ = openURL(ss.URL())
		return
	}

	// Tray.
	t := tray.New(app)
	app.tray = t

	// Background loop: refresh tray + reconnect on demand / on schedule.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.cancelRoot = cancel
	go app.bridgeLoop(ctx)
	go app.tickerLoop(ctx)
	go signalLoop(ctx, cancel)

	// If we have no config yet, open settings on first launch.
	if cfg.HomeAssistantURL == "" || cfg.RefreshToken == "" {
		go func() {
			time.Sleep(800 * time.Millisecond)
			_ = openURL(ss.URL())
		}()
	}

	t.Run() // blocks until tray quits
	cancel()
	app.wg.Wait()
}

// App is the central coordinator. All fields are read by both the tray and
// the settings handlers; lock cfgMu before reading/writing cfg.
type App struct {
	cfg     *config.Config
	cfgMu   sync.Mutex
	audio   audio.Client

	tray     *tray.Tray
	settings *settings.Server

	wg sync.WaitGroup

	// kickReconnect signals the bridge loop to drop + rebuild. Closed once
	// on shutdown via cancelRoot.
	kickReconnect chan struct{}
	cancelRoot    context.CancelFunc

	connected  atomic.Bool
	statusMsg  atomic.Value // string

	// Authorize state: at most one in flight.
	authMu sync.Mutex
	authCh chan settings.AuthStatus
}

func (a *App) setStatus(s string) { a.statusMsg.Store(s) }
func (a *App) getStatus() string {
	if v, ok := a.statusMsg.Load().(string); ok {
		return v
	}
	return ""
}

// ---- Tray controller -------------------------------------------------------

func (a *App) Status() tray.Status {
	a.cfgMu.Lock()
	url := a.cfg.HomeAssistantURL
	a.cfgMu.Unlock()
	vol, _ := a.safeVolume()
	mut, _ := a.safeMuted()
	dev := ""
	if a.audio != nil {
		dev = a.audio.DeviceName()
	}
	return tray.Status{
		Connected:  a.connected.Load(),
		Volume:     vol,
		Muted:      mut,
		DeviceName: dev,
		StatusText: a.getStatus(),
		HassURL:    url,
	}
}

func (a *App) OpenSettings()       { _ = openURL(a.settings.URL()) }
func (a *App) OpenHomeAssistant()  {
	a.cfgMu.Lock()
	u := a.cfg.HomeAssistantURL
	a.cfgMu.Unlock()
	if u != "" {
		_ = openURL(u)
	}
}
func (a *App) Reconnect() { a.kickReconnectOnce() }
func (a *App) Quit()      { a.cancelRoot(); a.tray.Quit() }

// ---- Settings controller --------------------------------------------------

func (a *App) GetConfig() *config.Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	c := *a.cfg
	return &c
}

// SaveConfig merges the page-editable fields into the live config and
// persists. Authentication state (URL, refresh token, client_id) is NOT
// touched here — clear it via Disconnect.
func (a *App) SaveConfig(c *config.Config) error {
	a.cfgMu.Lock()
	a.cfg.EntityVolume = c.EntityVolume
	a.cfg.EntityMuted = c.EntityMuted
	a.cfg.Step = c.Step
	err := config.Save(a.cfg)
	a.cfgMu.Unlock()
	return err
}

// Disconnect forgets the saved HA URL and refresh token, then kicks the
// bridge loop so the UI shows the reconnect-needed state.
func (a *App) Disconnect() error {
	a.cfgMu.Lock()
	a.cfg.HomeAssistantURL = ""
	a.cfg.RefreshToken = ""
	a.cfg.ClientID = ""
	err := config.Save(a.cfg)
	a.cfgMu.Unlock()
	a.kickReconnectOnce()
	return err
}

func (a *App) GetStatus() settings.Status {
	v, _ := a.safeVolume()
	m, _ := a.safeMuted()
	dev := ""
	if a.audio != nil {
		dev = a.audio.DeviceName()
	}
	return settings.Status{
		Connected:  a.connected.Load(),
		Volume:     v,
		Muted:      m,
		DeviceName: dev,
		Message:    a.getStatus(),
	}
}

func (a *App) SetRunAtStartup(enable bool) error {
	if err := startup.SetEnabled(enable); err != nil {
		return err
	}
	a.cfgMu.Lock()
	a.cfg.RunAtStartup = enable
	err := config.Save(a.cfg)
	a.cfgMu.Unlock()
	return err
}

// IsRunAtStartup returns the registry's truth, not the cached config value,
// so the toggle can detect external edits.
func (a *App) IsRunAtStartup() bool {
	on, err := startup.IsEnabled()
	if err != nil {
		return false
	}
	return on
}

// StartAuthorize runs the OAuth flow against hassURL. The returned channel
// streams progress strings (open in browser, exchanging code, etc.) and is
// closed when the flow ends. Only one authorize may be in flight at a time.
func (a *App) StartAuthorize(hassURL string) (chan settings.AuthStatus, error) {
	a.authMu.Lock()
	if a.authCh != nil {
		a.authMu.Unlock()
		return nil, fmt.Errorf("authorization already in progress")
	}
	ch := make(chan settings.AuthStatus, 8)
	a.authCh = ch
	a.authMu.Unlock()

	go func() {
		defer func() {
			a.authMu.Lock()
			a.authCh = nil
			a.authMu.Unlock()
			close(ch)
		}()
		emit := func(stage, text string) {
			select {
			case ch <- settings.AuthStatus{Stage: stage, Text: text}:
			default:
			}
		}
		a.setStatus("Authorising with " + hassURL + "…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		tok, err := haclient.Authorize(ctx, haclient.AuthorizeOptions{
			HassURL:  hassURL,
			OnStatus: func(s string) { emit("info", s) },
		})
		if err != nil {
			a.setStatus("Error: " + err.Error())
			emit("error", err.Error())
			return
		}
		a.cfgMu.Lock()
		a.cfg.HomeAssistantURL = hassURL
		a.cfg.RefreshToken = tok.RefreshToken
		a.cfg.ClientID = tok.ClientID
		_ = config.Save(a.cfg)
		a.cfgMu.Unlock()
		emit("success", "Connected. Setting up entities…")
		a.kickReconnectOnce()
	}()
	return ch, nil
}

// ---- internal loops --------------------------------------------------------

func (a *App) kickReconnectOnce() {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.kickReconnect != nil {
		select {
		case a.kickReconnect <- struct{}{}:
		default:
		}
	}
}

// bridgeLoop runs the audio↔HA bridge with reconnect backoff. Cancels when
// ctx is done.
func (a *App) bridgeLoop(ctx context.Context) {
	a.wg.Add(1)
	defer a.wg.Done()
	a.cfgMu.Lock()
	a.kickReconnect = make(chan struct{}, 1)
	kick := a.kickReconnect
	a.cfgMu.Unlock()

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		cfg := a.snapshotCfg()
		if cfg.HomeAssistantURL == "" || cfg.RefreshToken == "" || a.audio == nil {
			a.connected.Store(false)
			a.setStatus("Not connected — open Settings to authorise")
			a.tray.Refresh()
			select {
			case <-ctx.Done():
				return
			case <-kick:
				continue
			}
		}

		a.setStatus("Refreshing access token…")
		a.tray.Refresh()
		tok, err := haclient.Refresh(ctx, cfg.HomeAssistantURL, cfg.ClientID, cfg.RefreshToken)
		if err != nil {
			a.connected.Store(false)
			a.setStatus("Token refresh failed: " + err.Error())
			a.tray.Refresh()
			logx.Errorf("token refresh: %v", err)
			if waitOrKick(ctx, kick, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		a.setStatus("Connecting…")
		a.tray.Refresh()
		runCtx, runCancel := context.WithCancel(ctx)
		bridgeDone := make(chan error, 1)
		go func() {
			err := bridge.Run(runCtx, cfg, tok.AccessToken, a.audio)
			bridgeDone <- err
		}()

		// Mark connected once Run has had a moment to set things up.
		// (bridge logs "ready" once entities are sorted.)
		time.AfterFunc(2*time.Second, func() {
			if runCtx.Err() == nil {
				a.connected.Store(true)
				a.setStatus("Connected to " + cfg.HomeAssistantURL)
				a.tray.Refresh()
			}
		})

		select {
		case <-ctx.Done():
			runCancel()
			<-bridgeDone
			return
		case <-kick:
			runCancel()
			<-bridgeDone
			backoff = time.Second
			continue
		case err := <-bridgeDone:
			runCancel()
			a.connected.Store(false)
			if err != nil {
				a.setStatus("Disconnected: " + err.Error())
				logx.Warnf("bridge: %v", err)
			} else {
				a.setStatus("Disconnected")
			}
			a.tray.Refresh()
			if waitOrKick(ctx, kick, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// tickerLoop keeps the tray status fresh.
func (a *App) tickerLoop(ctx context.Context) {
	a.wg.Add(1)
	defer a.wg.Done()
	t := time.NewTicker(750 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if a.tray != nil {
				a.tray.Refresh()
			}
		}
	}
}

func (a *App) snapshotCfg() *config.Config {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	c := *a.cfg
	return &c
}

func (a *App) safeVolume() (int, error) {
	if a.audio == nil {
		return 0, nil
	}
	return a.audio.Volume()
}

func (a *App) safeMuted() (bool, error) {
	if a.audio == nil {
		return false, nil
	}
	return a.audio.Muted()
}

// waitOrKick blocks for d or until kick fires / ctx ends. Returns true if
// ctx ended.
func waitOrKick(ctx context.Context, kick chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-kick:
		return false
	case <-t.C:
		return false
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 60*time.Second {
		return 60 * time.Second
	}
	return d
}

// openURL is a tiny adapter so we don't import haclient just for this in
// many places. Kept in main.go because main is already wired to all packages.
func openURL(url string) error { return haclient.OpenBrowser(url) }

func signalLoop(ctx context.Context, cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	select {
	case <-ctx.Done():
	case <-ch:
		logx.Infof("signal received, exiting")
		cancel()
	}
}
