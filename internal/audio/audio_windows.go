//go:build windows

package audio

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// pollInterval is how often we re-read the endpoint to detect external
// changes (volume keys, the Windows mixer). The endpoint exposes a
// notification callback, but go-wca's binding for it doesn't accept a
// callback object, so polling is the pragmatic option. 250 ms is well below
// the threshold a user would notice on a slider.
const pollInterval = 250 * time.Millisecond

type winClient struct {
	enum       *wca.IMMDeviceEnumerator
	device     *wca.IMMDevice
	endpoint   *wca.IAudioEndpointVolume
	deviceName string

	mu     sync.Mutex
	closed bool
}

func openImpl() (Client, error) {
	// Single-threaded apartment is fine for our usage — we'll only touch
	// these interfaces from the goroutine that owns them via channels.
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// CO_E_ALREADYINITIALIZED is fine; anything else is fatal.
		if oleErr, ok := err.(*ole.OleError); !ok || oleErr.Code() != 0x800401F0 {
			// Note: returning here only if it's an actual failure; the
			// "already initialised" path falls through.
			// 0x800401F0 = CO_E_NOTINITIALIZED ; the real "already" code
			// is S_FALSE (1) which CoInitializeEx returns without error.
			// Either way, treating any error as recoverable is safe because
			// we'll detect a missing enumerator below.
			_ = oleErr
		}
	}

	var enum *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&enum,
	); err != nil {
		return nil, fmt.Errorf("MMDeviceEnumerator: %w", err)
	}

	c := &winClient{enum: enum}
	if err := c.refreshDefault(); err != nil {
		enum.Release()
		return nil, err
	}
	return c, nil
}

// refreshDefault re-acquires the default render endpoint. Called on Open and
// whenever the device appears to have changed (e.g. headphones unplugged).
func (c *winClient) refreshDefault() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Release any previous handles.
	if c.endpoint != nil {
		c.endpoint.Release()
		c.endpoint = nil
	}
	if c.device != nil {
		c.device.Release()
		c.device = nil
	}

	var dev *wca.IMMDevice
	if err := c.enum.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &dev); err != nil {
		return fmt.Errorf("GetDefaultAudioEndpoint: %w", err)
	}
	var ep *wca.IAudioEndpointVolume
	if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &ep); err != nil {
		dev.Release()
		return fmt.Errorf("Activate(IAudioEndpointVolume): %w", err)
	}
	c.device = dev
	c.endpoint = ep
	c.deviceName = readDeviceName(dev)
	return nil
}

// readDeviceName tries to extract the friendly name. Best-effort; an empty
// string is fine — we only use it for diagnostics.
func readDeviceName(dev *wca.IMMDevice) string {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return ""
	}
	defer ps.Release()
	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return ""
	}
	return pv.String()
}

func (c *winClient) Volume() (int, error) {
	c.mu.Lock()
	ep := c.endpoint
	c.mu.Unlock()
	if ep == nil {
		return 0, fmt.Errorf("audio: no endpoint")
	}
	var lvl float32
	if err := ep.GetMasterVolumeLevelScalar(&lvl); err != nil {
		return 0, err
	}
	return int(math.Round(float64(lvl) * 100)), nil
}

func (c *winClient) SetVolume(percent int) error {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	c.mu.Lock()
	ep := c.endpoint
	c.mu.Unlock()
	if ep == nil {
		return fmt.Errorf("audio: no endpoint")
	}
	return ep.SetMasterVolumeLevelScalar(float32(percent)/100, nil)
}

func (c *winClient) Muted() (bool, error) {
	c.mu.Lock()
	ep := c.endpoint
	c.mu.Unlock()
	if ep == nil {
		return false, fmt.Errorf("audio: no endpoint")
	}
	var m bool
	if err := ep.GetMute(&m); err != nil {
		return false, err
	}
	return m, nil
}

func (c *winClient) SetMuted(b bool) error {
	c.mu.Lock()
	ep := c.endpoint
	c.mu.Unlock()
	if ep == nil {
		return fmt.Errorf("audio: no endpoint")
	}
	return ep.SetMute(b, nil)
}

func (c *winClient) DeviceName() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deviceName
}

func (c *winClient) Watch(ctx context.Context) <-chan State {
	out := make(chan State, 4)
	go func() {
		defer close(out)
		// Emit initial state once.
		var prev State
		first := true
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			vol, vErr := c.Volume()
			mut, mErr := c.Muted()
			if vErr != nil || mErr != nil {
				// Likely a device change; try to recover.
				if rErr := c.refreshDefault(); rErr == nil {
					vol, vErr = c.Volume()
					mut, mErr = c.Muted()
				}
			}
			if vErr == nil && mErr == nil {
				cur := State{Volume: vol, Muted: mut}
				if first || cur != prev {
					select {
					case out <- cur:
					case <-ctx.Done():
						return
					}
					prev = cur
					first = false
				}
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (c *winClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.endpoint != nil {
		c.endpoint.Release()
	}
	if c.device != nil {
		c.device.Release()
	}
	if c.enum != nil {
		c.enum.Release()
	}
	ole.CoUninitialize()
	return nil
}
