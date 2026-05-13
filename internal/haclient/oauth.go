// Package haclient implements the Home Assistant authentication and WebSocket
// transport. This file is the OAuth half: the on-device OAuth-for-native-apps
// flow (RFC 8252) using PKCE, with the user's default browser plus a local
// loopback redirect.
package haclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Tokens is what the OAuth flow yields. We persist RefreshToken + ClientID;
// AccessToken / ExpiresAt are runtime-only.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	// ClientID is the same loopback URL we presented at authorize time. HA
	// stores this with the refresh token and rejects refresh requests with a
	// different client_id, so we have to round-trip it.
	ClientID string `json:"client_id,omitempty"`
}

// Valid reports whether the access token is still usable with a small safety
// margin so we refresh proactively rather than racing the server.
func (t *Tokens) Valid() bool {
	return t != nil && t.AccessToken != "" && time.Until(t.ExpiresAt) > 30*time.Second
}

// browserOpener is the function used to launch a URL. Overridable in tests.
var browserOpener = openBrowser

// AuthorizeOptions configures the OAuth flow.
type AuthorizeOptions struct {
	// HassURL is the base URL of the Home Assistant instance, e.g.
	// "https://hass.example.com:8123". Trailing slash optional.
	HassURL string

	// OnStatus is called with human-readable progress strings for the UI.
	OnStatus func(s string)
}

// We derive the OAuth client_id from the loopback redirect URI so they share
// scheme + netloc — Home Assistant's IndieAuth check (verify_redirect_uri)
// then short-circuits at stage 1 with no need for HA to fetch the client_id
// URL. The client_id is persisted with the refresh token so we can pass it
// back unchanged on /auth/token refresh.

// Authorize runs the full PKCE flow:
//
//  1. Spin up a loopback HTTP server on 127.0.0.1:<random>.
//  2. Open the user's browser to HA's /auth/authorize.
//  3. Receive the authorization code on the local callback.
//  4. Exchange the code at /auth/token.
//
// Blocks until the user completes or aborts. Cancel ctx to abort.
func Authorize(ctx context.Context, opts AuthorizeOptions) (*Tokens, error) {
	if opts.HassURL == "" {
		return nil, errors.New("haclient: HassURL is required")
	}
	status := opts.OnStatus
	if status == nil {
		status = func(string) {}
	}
	base := strings.TrimRight(opts.HassURL, "/")

	// Loopback listener. The same scheme+netloc becomes our client_id so
	// HA's IndieAuth check passes at stage 1 (no client_id fetch).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	clientID := fmt.Sprintf("http://127.0.0.1:%d/", port)
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/cb", port)

	// PKCE.
	verifier, challenge, err := pkcePair()
	if err != nil {
		ln.Close()
		return nil, err
	}
	state, err := randB64(16)
	if err != nil {
		ln.Close()
		return nil, err
	}

	// Capture code via channel.
	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	var once sync.Once
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cb" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if q.Get("state") != state {
				writeHTML(w, "Authentication failed", "State mismatch — possible CSRF.")
				once.Do(func() { ch <- result{err: errors.New("state mismatch")} })
				return
			}
			if e := q.Get("error"); e != "" {
				writeHTML(w, "Authentication failed", html.EscapeString(e))
				once.Do(func() { ch <- result{err: fmt.Errorf("authorize error: %s", e)} })
				return
			}
			code := q.Get("code")
			if code == "" {
				writeHTML(w, "Authentication failed", "No code returned.")
				once.Do(func() { ch <- result{err: errors.New("no code")} })
				return
			}
			writeHTML(w, "Connected!",
				"You can close this tab and return to the app.")
			once.Do(func() { ch <- result{code: code} })
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())

	authURL := base + "/auth/authorize?" + url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	status("Opening browser…")
	if err := browserOpener(authURL); err != nil {
		// Not fatal — the user can also paste the URL manually. Surface it.
		status("Open this URL manually: " + authURL)
	} else {
		status("Waiting for authorization in browser…")
	}

	var code string
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		code = r.code
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return nil, errors.New("authorize: timed out")
	}

	status("Exchanging code for token…")
	tok, err := exchangeCode(ctx, base, clientID, redirectURI, code, verifier)
	if err != nil {
		return nil, err
	}
	tok.ClientID = clientID
	return tok, nil
}

// exchangeCode does the OAuth token-endpoint POST.
func exchangeCode(ctx context.Context, base, clientID, redirectURI, code, verifier string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	return doTokenRequest(ctx, base, form)
}

// Refresh exchanges a refresh token for a fresh access token. HA does not
// rotate the refresh token on refresh, so callers reuse the saved one.
// clientID must be the same loopback URL used during Authorize.
func Refresh(ctx context.Context, hassURL, clientID, refreshToken string) (*Tokens, error) {
	if clientID == "" {
		return nil, errors.New("haclient: client_id required for refresh")
	}
	base := strings.TrimRight(hassURL, "/")
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	t, err := doTokenRequest(ctx, base, form)
	if err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	t.ClientID = clientID
	return t, nil
}

func doTokenRequest(ctx context.Context, base string, form url.Values) (*Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/auth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode token: %w (body=%s)", err, string(body))
	}
	return &Tokens{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}, nil
}

// pkcePair generates a verifier and its S256 challenge per RFC 7636.
func pkcePair() (verifier, challenge string, err error) {
	verifier, err = randB64(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func writeHTML(w http.ResponseWriter, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>
  body{font-family:system-ui,sans-serif;background:#0f1419;color:#e6edf3;
       display:flex;align-items:center;justify-content:center;height:100vh;margin:0;}
  .card{max-width:480px;padding:32px;background:#161b22;border:1px solid #30363d;border-radius:12px;text-align:center}
  h1{margin:0 0 12px 0;font-size:20px}
  p{color:#8b949e;margin:0;font-size:14px}
</style></head><body>
<div class="card"><h1>%s</h1><p>%s</p></div>
</body></html>`, title, title, msg)
}
