package audio

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// ErrDeviceStalled means the input device stopped delivering audio (for
// example, it was unplugged or the system revoked access).
var ErrDeviceStalled = errors.New("microphone stopped delivering audio")

// Device is an input device as presented to people.
type Device struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"default"`
}

// InputDevices lists capture devices.
func InputDevices() ([]Device, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()
	infos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(infos))
	for _, info := range infos {
		out = append(out, Device{Name: info.Name(), IsDefault: info.IsDefault != 0})
	}
	return out, nil
}

// Mic captures the microphone at 16 kHz mono into a bounded ring buffer.
// The audio callback never blocks: if the reader falls more than ten
// seconds behind, the oldest audio is dropped.
type Mic struct {
	ctx  *malgo.AllocatedContext
	dev  *malgo.Device
	name string

	mu      sync.Mutex
	cond    *sync.Cond
	ring    []float32
	head    int // read index
	size    int // samples available
	closed  bool
	stopped bool
	dropped int
}

const ringSeconds = 10

// OpenMic opens the named input device ("" = system default) and starts it.
func OpenMic(device string) (*Mic, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("audio system unavailable: %w", err)
	}
	m := &Mic{ctx: ctx, ring: make([]float32, ringSeconds*SampleRate)}
	m.cond = sync.NewCond(&m.mu)
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatF32
	cfg.Capture.Channels = 1
	cfg.SampleRate = SampleRate
	cfg.PeriodSizeInMilliseconds = 20
	cfg.Alsa.NoMMap = 1
	var chosen *malgo.DeviceInfo
	if device != "" {
		infos, err := ctx.Devices(malgo.Capture)
		if err != nil {
			m.free()
			return nil, err
		}
		for i := range infos {
			if strings.EqualFold(infos[i].Name(), device) {
				chosen = &infos[i]
				break
			}
		}
		if chosen == nil {
			m.free()
			return nil, fmt.Errorf("input device %q not found", device)
		}
		cfg.Capture.DeviceID = chosen.ID.Pointer()
		m.name = chosen.Name()
	}
	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: m.onData,
		Stop: m.onStop,
	})
	if err != nil {
		m.free()
		return nil, fmt.Errorf("cannot open microphone: %w", err)
	}
	m.dev = dev
	if err := dev.Start(); err != nil {
		m.free()
		return nil, fmt.Errorf("cannot start microphone: %w", err)
	}
	return m, nil
}

func (m *Mic) onData(_, input []byte, frames uint32) {
	if len(input) < int(frames)*4 || frames == 0 {
		return
	}
	samples := unsafe.Slice((*float32)(unsafe.Pointer(&input[0])), frames)
	m.mu.Lock()
	n := len(m.ring)
	for _, s := range samples {
		if m.size == n { // reader stalled: drop the oldest sample
			m.head = (m.head + 1) % n
			m.size--
			m.dropped++
		}
		m.ring[(m.head+m.size)%n] = s
		m.size++
	}
	m.mu.Unlock()
	m.cond.Broadcast()
}

func (m *Mic) onStop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.cond.Broadcast()
}

// Read blocks until len(dst) samples are available. It fails if the device
// stops or delivers nothing for two seconds.
func (m *Mic) Read(dst []float32) error {
	deadline := time.Now().Add(2*time.Second + time.Duration(len(dst))*time.Second/SampleRate)
	timer := time.AfterFunc(time.Until(deadline), m.cond.Broadcast)
	defer timer.Stop()
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.size < len(dst) {
		if m.closed {
			return errors.New("microphone closed")
		}
		if m.stopped || time.Now().After(deadline) {
			return ErrDeviceStalled
		}
		m.cond.Wait()
	}
	n := len(m.ring)
	for i := range dst {
		dst[i] = m.ring[(m.head+i)%n]
	}
	m.head = (m.head + len(dst)) % n
	m.size -= len(dst)
	return nil
}

// Dropped reports samples discarded because the reader fell behind.
func (m *Mic) Dropped() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dropped
}

// Close stops and releases the device.
func (m *Mic) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cond.Broadcast()
	m.free()
	return nil
}

func (m *Mic) free() {
	if m.dev != nil {
		m.dev.Uninit()
		m.dev = nil
	}
	if m.ctx != nil {
		_ = m.ctx.Uninit()
		m.ctx.Free()
		m.ctx = nil
	}
}
