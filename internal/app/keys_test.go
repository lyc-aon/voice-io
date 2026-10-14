package app

import (
	"slices"
	"sync"
	"testing"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/platform"
)

type sent struct {
	mu   sync.Mutex
	cmds []string
}

func (s *sent) send(cmd string) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
}

func (s *sent) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.cmds)
}

func gestures(idle, hold bool) (*keyGestures, *sent) {
	s := &sent{}
	return &keyGestures{send: s.send, idle: func() bool { return idle }, holdToTalk: func() bool { return hold }}, s
}

func expect(t *testing.T, s *sent, want ...string) {
	t.Helper()
	if got := s.get(); !slices.Equal(got, want) {
		t.Fatalf("commands %q; want %q", got, want)
	}
}

func TestFnTapDoesNothingAndHoldDictates(t *testing.T) {
	k, s := gestures(true, true)
	k.fn(platform.FnPressed)
	time.Sleep(fnHoldDelay / 4)
	k.fn(platform.FnReleased)
	time.Sleep(fnHoldDelay)
	expect(t, s) // a tap stays the system's 🌐 action

	k.fn(platform.FnPressed)
	time.Sleep(fnHoldDelay + 120*time.Millisecond)
	expect(t, s, "dictate")
	k.fn(platform.FnReleased)
	expect(t, s, "dictate", "dictate") // release finishes the held session
}

func TestFnChordDiscardsTheRecordingItStarted(t *testing.T) {
	k, s := gestures(true, true)
	k.fn(platform.FnPressed)
	k.fn(platform.FnChord) // fn+key before the hold delay: a shortcut, not dictation
	time.Sleep(fnHoldDelay + 60*time.Millisecond)
	expect(t, s)

	k.fn(platform.FnPressed)
	time.Sleep(fnHoldDelay + 120*time.Millisecond)
	k.fn(platform.FnChord)
	k.fn(platform.FnReleased)
	expect(t, s, "dictate", "cancel")
}

func TestFnIsIgnoredWhileASessionIsActive(t *testing.T) {
	k, s := gestures(false, true)
	k.fn(platform.FnPressed)
	time.Sleep(fnHoldDelay + 120*time.Millisecond)
	k.fn(platform.FnReleased)
	expect(t, s)
}

func TestHoldToTalkFinishesOnReleaseOnlyAfterAHold(t *testing.T) {
	k, s := gestures(true, true)
	k.hotkey(platform.ActionPush, true)
	k.hotkey(platform.ActionPush, false) // a tap starts a hands-free session
	expect(t, s, "push")

	k, s = gestures(true, true)
	k.hotkey(platform.ActionPush, true)
	time.Sleep(holdThreshold + 50*time.Millisecond)
	k.hotkey(platform.ActionPush, false)
	expect(t, s, "push", "push")

	k, s = gestures(true, false) // hold to talk off: release never finishes
	k.hotkey(platform.ActionPush, true)
	time.Sleep(holdThreshold + 50*time.Millisecond)
	k.hotkey(platform.ActionPush, false)
	expect(t, s, "push")

	k, s = gestures(false, true) // the press that finishes a session
	k.hotkey(platform.ActionPush, true)
	time.Sleep(holdThreshold + 50*time.Millisecond)
	k.hotkey(platform.ActionPush, false)
	expect(t, s, "push")
}

func TestPausedShortcutsSendNothing(t *testing.T) {
	k, s := gestures(true, true)
	k.setPaused(true)
	k.hotkey(platform.ActionPush, true)
	k.hotkey(platform.ActionToggle, true)
	k.fn(platform.FnPressed)
	time.Sleep(fnHoldDelay + 60*time.Millisecond)
	k.fn(platform.FnReleased)
	expect(t, s)
	k.setPaused(false)
	k.hotkey(platform.ActionCancel, true)
	k.hotkey(platform.ActionCancel, false)
	expect(t, s, "cancel")
}
