// Package logx is a tiny structured log helper. We don't ship a UI for logs,
// so the goal is: predictable text lines, a ring buffer that the settings page
// can dump, and a file in %APPDATA% for after-the-fact debugging.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const ringSize = 500

var (
	mu      sync.Mutex
	ring    [ringSize]string
	ringPos int
	ringN   int

	fileW io.Writer
)

// Init opens a log file under dir (created if needed) and tees output there
// plus stderr.
func Init(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "ha-volume.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fileW = f
	log.SetOutput(io.MultiWriter(os.Stderr, f, ringWriter{}))
	log.SetFlags(0)
	return nil
}

type ringWriter struct{}

func (ringWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	ring[ringPos] = strings.TrimRight(string(p), "\n")
	ringPos = (ringPos + 1) % ringSize
	if ringN < ringSize {
		ringN++
	}
	return len(p), nil
}

// Snapshot returns the current ring buffer oldest-to-newest.
func Snapshot() []string {
	mu.Lock()
	defer mu.Unlock()
	out := make([]string, 0, ringN)
	start := ringPos - ringN
	if start < 0 {
		start += ringSize
	}
	for i := 0; i < ringN; i++ {
		out = append(out, ring[(start+i)%ringSize])
	}
	return out
}

// Infof / Warnf / Errorf write a single line. Format is `LEVEL TS message`
// so the settings page can pretty-print without parsing JSON.
func Infof(format string, args ...any)  { write("INFO", format, args...) }
func Warnf(format string, args ...any)  { write("WARN", format, args...) }
func Errorf(format string, args ...any) { write("ERROR", format, args...) }

func write(level, format string, args ...any) {
	log.Printf("%-5s %s %s", level, time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
