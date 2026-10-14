// Package meeting captures microphone-only meetings to private WAV files,
// produces speaker-labelled transcripts and optional OpenAI-compatible
// summaries, and formats collaborative drafts.
package meeting

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitlab.com/integral-inference/voice-io/internal/audio"
	"gitlab.com/integral-inference/voice-io/internal/paths"
)

// Recorder streams one session to disk so memory stays bounded to one block.
// The WAV header is kept current after every append, so an interrupted
// recording remains a readable file.
type Recorder struct {
	Dir     string
	Path    string
	f       *os.File
	samples int64
	synced  int64
	pcm     []byte
}

// NewRecorder creates a private recording in dir (paths.MeetingsDir() when empty).
func NewRecorder(dir string) (*Recorder, error) {
	if dir == "" {
		dir = paths.MeetingsDir()
	}
	if err := paths.PrivateDir(dir); err != nil {
		return nil, fmt.Errorf("meeting directory unavailable: %w", err)
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	name := time.Now().UTC().Format("2006-01-02-150405") + "-" + hex.EncodeToString(suffix[:]) + ".wav"
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(audio.WAVHeader(audio.SampleRate, 0)); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return &Recorder{Dir: dir, Path: path, f: f}, nil
}

// Samples is the number of recorded samples.
func (r *Recorder) Samples() int64 { return r.samples }

// Append writes one block and refreshes the header; data is synced to disk
// every five seconds of audio.
func (r *Recorder) Append(samples []float32) error {
	if r.f == nil {
		return errors.New("meeting recording already closed")
	}
	r.pcm = audio.PCM16(r.pcm[:0], samples)
	if _, err := r.f.Write(r.pcm); err != nil {
		return err
	}
	r.samples += int64(len(samples))
	if err := r.patchHeader(); err != nil {
		return err
	}
	if r.samples-r.synced >= 5*audio.SampleRate {
		if err := r.f.Sync(); err != nil {
			return err
		}
		r.synced = r.samples
	}
	return nil
}

func (r *Recorder) patchHeader() error {
	data := uint32(r.samples * 2)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], 36+data)
	if _, err := r.f.WriteAt(b[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(b[:], data)
	_, err := r.f.WriteAt(b[:], 40)
	return err
}

// Close finalizes and syncs the file, returning its path.
func (r *Recorder) Close() (string, error) {
	if r.f == nil {
		return r.Path, nil
	}
	err := r.patchHeader()
	if serr := r.f.Sync(); err == nil {
		err = serr
	}
	if cerr := r.f.Close(); err == nil {
		err = cerr
	}
	r.f = nil
	return r.Path, err
}

// Discard closes and deletes the recording.
func (r *Recorder) Discard() error {
	_, err := r.Close()
	if rerr := os.Remove(r.Path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	return err
}
