package haclient

import (
	"os/exec"
	"runtime"
)

// OpenBrowser launches url in the user's default browser. Exported so the
// main package can reuse it for the systray's "Open Home Assistant" /
// "Open Settings" actions.
func OpenBrowser(url string) error { return openBrowser(url) }

// openBrowser launches url in the user's default browser. The implementation
// is intentionally trivial — we only need to handle Windows for the shipped
// binary, but darwin/linux fallbacks keep dev builds usable.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		// `rundll32 url.dll,FileProtocolHandler` is the canonical handler
		// invocation that doesn't open a console window. `cmd /c start` works
		// too but flashes a console.
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
