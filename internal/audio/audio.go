// Package audio provides 16 kHz mono float32 sources (microphone, WAV file)
// plus WAV encoding and the overlay's speech-level scale.
package audio

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

// SampleRate is the rate every model expects.
const SampleRate = 16000

// Source yields mono float32 samples at SampleRate.
type Source interface {
	// Read fills dst completely or returns an error (io.EOF at the end of a file).
	Read(dst []float32) error
	Close() error
}

// RMS is the root-mean-square amplitude of samples.
func RMS(samples []float32) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(samples)))
}

// SpeechLevel maps an RMS above the noise floor to a display level: -60 dBFS
// (residual room noise) is 0, ordinary speech around -30 dBFS is 0.75 and
// -20 dBFS fills the meter.
func SpeechLevel(rms float64) float64 {
	if rms <= 0 {
		return 0
	}
	return math.Min(1, math.Max(0, (20*math.Log10(rms)+60)/40))
}

// ReadWAV decodes a mono 16-bit PCM or 32-bit float WAV file.
func ReadWAV(path string) ([]float32, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r, err := NewWAVReader(f)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	samples := make([]float32, 0, r.Frames())
	buf := make([]float32, 4096)
	for {
		n, err := r.ReadSamples(buf)
		samples = append(samples, buf[:n]...)
		if err == io.EOF {
			return samples, r.Rate, nil
		}
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", path, err)
		}
	}
}

// WAVReader streams samples from a WAV file.
type WAVReader struct {
	r        *bufio.Reader
	Rate     int
	bits     int
	float    bool
	remain   int64 // bytes of sample data left
	dataSize int64
	raw      []byte
}

// NewWAVReader parses RIFF headers up to the data chunk.
func NewWAVReader(src io.Reader) (*WAVReader, error) {
	r := bufio.NewReaderSize(src, 64<<10)
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, errors.New("not a WAV file")
	}
	if string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	w := &WAVReader{r: r}
	haveFmt := false
	for {
		var ch [8]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return nil, errors.New("WAV file has no data chunk")
		}
		id := string(ch[0:4])
		size := int64(binary.LittleEndian.Uint32(ch[4:8]))
		switch id {
		case "fmt ":
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil || size < 16 {
				return nil, errors.New("truncated WAV fmt chunk")
			}
			format := binary.LittleEndian.Uint16(body[0:2])
			channels := binary.LittleEndian.Uint16(body[2:4])
			w.Rate = int(binary.LittleEndian.Uint32(body[4:8]))
			w.bits = int(binary.LittleEndian.Uint16(body[14:16]))
			if format == 0xFFFE && size >= 26 { // WAVE_FORMAT_EXTENSIBLE
				format = binary.LittleEndian.Uint16(body[24:26])
			}
			if channels != 1 {
				return nil, fmt.Errorf("expected mono audio, got %d channels", channels)
			}
			switch {
			case format == 1 && w.bits == 16:
			case format == 3 && w.bits == 32:
				w.float = true
			default:
				return nil, fmt.Errorf("unsupported WAV encoding (format %d, %d bits); use 16-bit PCM", format, w.bits)
			}
			haveFmt = true
			if size%2 == 1 {
				_, _ = r.ReadByte()
			}
		case "data":
			if !haveFmt {
				return nil, errors.New("WAV data precedes fmt chunk")
			}
			w.remain, w.dataSize = size, size
			return w, nil
		default:
			if _, err := io.CopyN(io.Discard, r, size+size%2); err != nil {
				return nil, errors.New("truncated WAV file")
			}
		}
	}
}

// Frames is the number of samples declared by the header.
func (w *WAVReader) Frames() int64 { return w.dataSize / int64(w.bits/8) }

// ReadSamples decodes up to len(dst) samples.
func (w *WAVReader) ReadSamples(dst []float32) (int, error) {
	if w.remain <= 0 {
		return 0, io.EOF
	}
	width := w.bits / 8
	want := min(int64(len(dst)*width), w.remain)
	want -= want % int64(width)
	if want == 0 {
		return 0, io.EOF
	}
	if cap(w.raw) < int(want) {
		w.raw = make([]byte, want)
	}
	raw := w.raw[:want]
	n, err := io.ReadFull(w.r, raw)
	n -= n % width
	w.remain -= int64(n)
	count := n / width
	for i := range count {
		if w.float {
			dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		} else {
			dst[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:]))) / 32768
		}
	}
	if err == io.ErrUnexpectedEOF || (err == io.EOF && count > 0) {
		w.remain = 0
		return count, nil
	}
	return count, err
}

// PCM16 converts float samples to clipped little-endian 16-bit PCM.
func PCM16(dst []byte, samples []float32) []byte {
	for _, s := range samples {
		v := math.Max(-1, math.Min(1, float64(s)))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(int16(math.Round(v*32767))))
	}
	return dst
}

// WAVHeader returns a 44-byte header for mono 16-bit PCM with dataBytes of samples.
func WAVHeader(rate int, dataBytes uint32) []byte {
	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, 36+dataBytes)
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, 1) // mono
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate*2))
	h = binary.LittleEndian.AppendUint16(h, 2)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	return binary.LittleEndian.AppendUint32(h, dataBytes)
}

// EncodeWAV returns a complete mono 16-bit WAV.
func EncodeWAV(samples []float32, rate int) []byte {
	out := WAVHeader(rate, uint32(len(samples)*2))
	return PCM16(out, samples)
}

// WriteWAV writes samples as a private (0600) mono 16-bit WAV file.
func WriteWAV(path string, samples []float32, rate int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(EncodeWAV(samples, rate)); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// FileSource replays a 16 kHz mono WAV as if it were a microphone. With
// Realtime it paces reads to wall-clock time; after the file it can append
// trailing silence so endpoints close naturally.
type FileSource struct {
	samples  []float32
	pos      int
	tail     int
	realtime bool
	start    time.Time
	read     int
}

// OpenFile loads a WAV file at SampleRate.
func OpenFile(path string, realtime bool, trailingSilence time.Duration) (*FileSource, error) {
	samples, rate, err := ReadWAV(path)
	if err != nil {
		return nil, err
	}
	if rate != SampleRate {
		return nil, fmt.Errorf("%s: expected %d Hz audio, got %d", path, SampleRate, rate)
	}
	return &FileSource{samples: samples, realtime: realtime,
		tail: int(trailingSilence.Seconds() * SampleRate)}, nil
}

func (f *FileSource) Read(dst []float32) error {
	if f.realtime {
		if f.start.IsZero() {
			f.start = time.Now()
		}
		due := f.start.Add(time.Duration(f.read+len(dst)) * time.Second / SampleRate)
		time.Sleep(time.Until(due))
	}
	total := len(f.samples) + f.tail
	if f.pos >= total {
		return io.EOF
	}
	for i := range dst {
		if f.pos < len(f.samples) {
			dst[i] = f.samples[f.pos]
		} else {
			dst[i] = 0
		}
		f.pos++
	}
	f.read += len(dst)
	return nil
}

func (f *FileSource) Close() error { return nil }
