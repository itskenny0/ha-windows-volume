# ha-volume

A tiny Windows tray app that mirrors the system master volume to Home Assistant
and lets HA control it from a dashboard slider.

- **Volume**: read+write the Windows default playback device's master volume.
- **Mute**: toggleable from HA.
- **No password handling**: authentication goes through your normal HA login
  page in your default browser (OAuth 2.0 with PKCE, RFC 8252 — same pattern
  as `gh`, `aws sso`, etc.).
- **No MQTT**: HA gets a real `input_number` helper that the app owns and
  syncs. A slider works out of the box in any dashboard.
- **Small**: ~7 MB self-contained `.exe`, no runtime to install.

## Build

```sh
make           # → bin/ha-volume.exe (Windows, GUI subsystem)
make test      # tests, including a mock-HA integration suite
make vet
```

Cross-compile from Linux needs nothing beyond Go 1.21+; CGO isn't used.

## Run

1. Launch `ha-volume.exe`. A speaker icon appears in the tray and your browser
   pops a small settings page (`http://127.0.0.1:<random>/`).
2. Paste your Home Assistant URL (e.g. `https://homeassistant.local:8123`) and
   click **Authorise…**. Your browser opens the HA login page; approve.
3. Done. The app creates `input_number.windows_volume_<host>` and
   `input_boolean.windows_muted_<host>` if they don't exist, and starts syncing.

To put a slider on a dashboard, just drop the `input_number` entity onto a
card — HA's default UI renders it as a slider (we set `mode: slider` when we
create the helper).

## What's stored, where

Per-user under `%APPDATA%\HAVolume\`:

| File          | Contents |
|---------------|----------|
| `config.json` | HA URL, refresh token, OAuth client_id, entity names, preferences. |
| `ha-volume.log` | Rolling text log. |

The refresh token grants long-lived access. To revoke, open HA → your profile
→ **Long-lived access tokens** / **Refresh tokens** and delete the entry, or
hit **Disconnect / reset** in the settings page.

## Layout

```
cmd/ha-volume/        entry point (wires everything)
internal/audio/       Windows Core Audio (go-wca, no CGO)
internal/haclient/    OAuth PKCE + WebSocket client
internal/bridge/      audio ↔ HA glue, echo suppression, entity provisioning
internal/config/      persistent state in %APPDATA%
internal/tray/        fyne.io/systray icon + menu (procedurally generated ICO)
internal/settings/    embedded HTML settings page on 127.0.0.1
internal/startup/     HKCU\…\Run registry toggle
internal/logx/        tiny ring-buffer + log file
```

## Design notes

**Why an `input_number`, not a `light`?** A controllable entity coming from a
remote app needs HA to know how to call back to *us* when the user moves the
slider. The cheap ways to do that are MQTT discovery (extra broker) or a
custom Python integration (not for a single-file app). The `input_number`
helper is owned by HA itself, has a slider in the UI, and is observable via
`state_changed` — that's all we need. The same approach gives us mute via
`input_boolean`.

**Why not a webview?** RFC 8252 says native apps should use the system
browser for OAuth so the user can reuse their session and tools (password
manager, 2FA, etc.). It also drops a Chromium/WebView2 dependency we'd
otherwise have to bundle.

**Echo suppression.** When the bridge pushes a value to HA (or vice versa),
it stamps the (value, time) pair. The matching `state_changed` event coming
back within 2 s is treated as our own echo and ignored. External changes —
which won't match the stamp — flow through as real commands.

**Polling vs notifications.** The Windows Core Audio API supports a
`IAudioEndpointVolumeCallback`, but go-wca's binding for it is a stub. We
poll the endpoint at 250 ms instead, which is well below human perception
on a slider.

## Tests

`go test ./...` exercises:

- PKCE verifier ↔ challenge.
- HA WebSocket auth, `get_states`, subscribe + receive event.
- End-to-end bridge: local→HA, HA→local, echo suppression — all against a
  mock HA WS server in-process.
