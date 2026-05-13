// Package settings serves a tiny local HTTP page for the user to configure
// the app: enter the HA URL, run the OAuth flow, change the entity names, or
// toggle "run at login." The page is served from 127.0.0.1 only and is bound
// to a randomly-chosen port to avoid conflicts. The tray menu's "Settings…"
// item opens the user's browser at the right URL.
package settings

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"ha-volume/internal/config"
	"ha-volume/internal/logx"
)

//go:embed page.html
var pageHTML string

// Controller is what the settings page needs from the rest of the app.
type Controller interface {
	GetConfig() *config.Config
	SaveConfig(c *config.Config) error
	StartAuthorize(hassURL string) (chan AuthStatus, error)
	Reconnect()
	Disconnect() error
	GetStatus() Status
	SetRunAtStartup(enable bool) error
	IsRunAtStartup() bool
}

// AuthStatus is the per-step status we stream back to the page during OAuth.
type AuthStatus struct {
	Stage string `json:"stage"` // info | success | error
	Text  string `json:"text"`
}

// Status mirrors the tray's view (used for the settings header).
type Status struct {
	Connected  bool   `json:"connected"`
	Volume     int    `json:"volume"`
	Muted      bool   `json:"muted"`
	DeviceName string `json:"device"`
	Message    string `json:"message"`
}

// Server is the running settings HTTP server.
type Server struct {
	c    Controller
	hs   *http.Server
	port int

	mu       sync.Mutex
	authChan chan AuthStatus
}

// Start spins up the server on a random loopback port and returns the URL
// that points at the index page.
func Start(c Controller) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{c: c, port: port}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/authorize", s.handleAuthorize)
	mux.HandleFunc("/api/authorize/stream", s.handleAuthorizeStream)
	mux.HandleFunc("/api/reconnect", s.handleReconnect)
	mux.HandleFunc("/api/startup", s.handleStartup)
	mux.HandleFunc("/api/disconnect", s.handleDisconnect)

	s.hs = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			logx.Errorf("settings server: %v", err)
		}
	}()
	logx.Infof("settings server listening on %s", s.URL())
	return s, nil
}

// URL returns the index URL for opening in a browser.
func (s *Server) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/", s.port)
}

// Stop shuts the server down. Safe to call multiple times.
func (s *Server) Stop() {
	if s.hs != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.hs.Shutdown(ctx)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(pageHTML))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.c.GetStatus())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.c.GetConfig()
		// Don't ship the refresh token to the page — no need.
		safe := *cfg
		safe.RefreshToken = mask(safe.RefreshToken)
		// Mirror the registry's truth into RunAtStartup so the toggle
		// reflects reality even if the user edited it externally.
		safe.RunAtStartup = s.c.IsRunAtStartup()
		writeJSON(w, http.StatusOK, safe)
	case http.MethodPost:
		cur := s.c.GetConfig()
		var in config.Config
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Only allow editing fields the page is supposed to touch.
		cur.EntityVolume = in.EntityVolume
		cur.EntityMuted = in.EntityMuted
		cur.Step = in.Step
		if err := s.c.SaveConfig(cur); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": logx.Snapshot(),
	})
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		HassURL string `json:"hass_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	in.HassURL = strings.TrimSpace(in.HassURL)
	if in.HassURL == "" {
		http.Error(w, "hass_url required", http.StatusBadRequest)
		return
	}
	ch, err := s.c.StartAuthorize(in.HassURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.authChan = ch
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthorizeStream pushes AuthStatus events to the page as SSE.
func (s *Server) handleAuthorizeStream(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ch := s.authChan
	s.mu.Unlock()
	if ch == nil {
		http.Error(w, "no authorization in progress", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for {
		select {
		case st, ok := <-ch:
			if !ok {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			b, _ := json.Marshal(st)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
			if st.Stage == "error" {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.c.Reconnect()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStartup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Enable bool `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.c.SetRunAtStartup(in.Enable); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.c.Disconnect(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-8) + s[len(s)-4:]
}

