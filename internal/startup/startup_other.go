//go:build !windows

package startup

// SetEnabled is a no-op on non-Windows.
func SetEnabled(enable bool) error { return nil }

// IsEnabled is always false on non-Windows.
func IsEnabled() (bool, error) { return false, nil }
