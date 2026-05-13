//go:build windows

package startup

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// runKey is the per-user registry path Windows scans on logon.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// SetEnabled writes (or deletes) our Run-key entry. The value is the
// quoted path to the running executable.
func SetEnabled(enable bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !enable {
		if err := k.DeleteValue(appName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Quote the path so a space-containing install dir doesn't bork Windows.
	return k.SetStringValue(appName, `"`+exe+`"`)
}

// IsEnabled reports whether our Run-key entry points at the current binary.
// Returns (false, nil) if the value is missing.
func IsEnabled() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(appName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.Trim(v, `"`), exe), nil
}
