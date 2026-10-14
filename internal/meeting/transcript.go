package meeting

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"gitlab.com/integral-inference/voice-io/internal/text"
)

// SpeechLine is one transcribed, speaker-labelled interval.
type SpeechLine struct {
	Start, End float64
	Speaker    int // diarizer cluster; negative = unknown
	Text       string
}

func clock(seconds float64) string {
	sec := int64(math.Max(0, math.RoundToEven(seconds)))
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, sec/60%60, sec%60)
}

// TranscriptText renders "[00:00:01–00:00:04] Speaker 1: text" lines.
// Speakers are numbered in order of first appearance.
func TranscriptText(lines []SpeechLine) string {
	labels := map[int]int{}
	out := make([]string, len(lines))
	for i, l := range lines {
		label := "Unknown speaker"
		if l.Speaker >= 0 {
			if _, ok := labels[l.Speaker]; !ok {
				labels[l.Speaker] = len(labels) + 1
			}
			label = fmt.Sprintf("Speaker %d", labels[l.Speaker])
		}
		out[i] = fmt.Sprintf("[%s–%s] %s: %s", clock(l.Start), clock(l.End), label, l.Text)
	}
	return strings.Join(out, "\n")
}

// CollaborativeText produces an editable speaker-labelled draft: each line
// is cleaned, contiguous chunks from one speaker merge, and speakers are
// numbered in order of first appearance.
func CollaborativeText(lines []SpeechLine, opts text.Options) string {
	var turns, parts []string
	var previous *SpeechLine
	speaker := -1
	labels := map[int]int{}
	appendTurn := func() {
		if len(parts) == 0 {
			return
		}
		label := "Unknown speaker"
		if speaker >= 0 {
			if _, ok := labels[speaker]; !ok {
				labels[speaker] = len(labels) + 1
			}
			label = fmt.Sprintf("Speaker %d", labels[speaker])
		}
		turns = append(turns, label+": "+strings.Join(parts, " "))
	}
	for i := range lines {
		line := &lines[i]
		cleaned := text.Cleanup(line.Text, opts)
		if cleaned == "" {
			appendTurn()
			parts, previous = nil, nil
			continue
		}
		if previous != nil {
			gap := line.Start - previous.End
			if line.Speaker != previous.Speaker || gap < 0 || gap > 0.05+1e-9 {
				appendTurn()
				parts = nil
			}
		}
		parts = append(parts, cleaned)
		speaker = line.Speaker
		previous = line
	}
	appendTurn()
	return strings.Join(turns, "\n")
}

// SummaryChars bounds each summary request.
const SummaryChars = 12000

// Chunks groups lines into blocks of at most limit characters without
// splitting a line.
func Chunks(lines []string, limit int) ([]string, error) {
	var groups, current []string
	size := 0
	for _, line := range lines {
		n := utf8.RuneCountInString(line)
		if n > limit {
			return nil, errors.New("meeting segment too long to summarize safely")
		}
		if len(current) > 0 && size+n+1 > limit {
			groups = append(groups, strings.Join(current, "\n"))
			current, size = nil, 0
		}
		current = append(current, line)
		size += n + 1
	}
	if len(current) > 0 {
		groups = append(groups, strings.Join(current, "\n"))
	}
	return groups, nil
}
