package app

import (
	"sync"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/platform"
)

// holdThreshold separates a held start shortcut (hold to talk) from a tap.
const holdThreshold = 400 * time.Millisecond

// fnHoldDelay separates a held fn (dictation) from a tap (the system's 🌐
// action): nothing starts until fn has been held this long, and the
// daemon seeds the session with a little pre-trigger audio so the first
// syllable survives the delay.
const fnHoldDelay = 180 * time.Millisecond

// keyGestures turns global shortcut and fn key events into daemon commands.
// A tap of the start shortcut starts or finishes a session; with hold to
// talk, releasing a held shortcut finishes the session it began. Holding fn
// starts dictation, releasing it finishes, and an fn+key chord discards the
// recording it began; a quick fn tap does nothing.
type keyGestures struct {
	send       func(cmd string)
	idle       func() bool // the daemon is idle (no session or processing)
	holdToTalk func() bool

	mu          sync.Mutex
	paused      bool // Settings is recording a new shortcut
	pushDownAt  time.Time
	pushStarted bool
	fnHeld      bool
	fnStarted   bool
	fnTimer     *time.Timer
}

func (k *keyGestures) setPaused(paused bool) {
	k.mu.Lock()
	k.paused = paused
	k.mu.Unlock()
}

func (k *keyGestures) isPaused() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.paused
}

func (k *keyGestures) hotkey(action platform.HotkeyAction, pressed bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.paused {
		return
	}
	switch action {
	case platform.ActionPush:
		if pressed {
			k.pushDownAt = time.Now()
			k.pushStarted = k.idle()
			k.send("push")
			return
		}
		if k.pushStarted && k.holdToTalk() && time.Since(k.pushDownAt) >= holdThreshold {
			k.send("push")
		}
		k.pushStarted = false
	case platform.ActionToggle:
		if pressed {
			k.send("toggle")
		}
	case platform.ActionCancel:
		if pressed {
			k.send("cancel")
		}
	}
}

func (k *keyGestures) fn(ev platform.FnEvent) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.paused {
		return
	}
	switch ev {
	case platform.FnPressed:
		if !k.idle() {
			return
		}
		k.fnHeld, k.fnStarted = true, false
		if k.fnTimer != nil {
			k.fnTimer.Stop()
		}
		k.fnTimer = time.AfterFunc(fnHoldDelay, func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.fnHeld && !k.fnStarted {
				k.fnStarted = true
				k.send("dictate")
			}
		})
	case platform.FnReleased, platform.FnChord:
		k.fnHeld = false
		if k.fnTimer != nil {
			k.fnTimer.Stop()
		}
		if !k.fnStarted {
			return
		}
		k.fnStarted = false
		if ev == platform.FnChord {
			k.send("cancel")
		} else {
			k.send("dictate") // finishes the held session
		}
	}
}
