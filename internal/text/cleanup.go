// Package text holds deterministic dictation cleanup and the guarded
// optional LLM polish prompt.
//
// Order: joined turn transcripts -> custom words -> filler removal ->
// joined-word respacing -> spoken emoji -> optional LLM polish -> guard.
package text

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Standalone English fillers; never substrings of real words.
var fillerRe = regexp.MustCompile(`(?i)\b(?:um|uh|mm|hmm|erm)\b`)

// Spoken emoji phrases mapped to glyphs.
var emoji = []struct{ phrase, glyph string }{
	{"thumbs up emoji", "\U0001F44D"},
	{"thumbs down emoji", "\U0001F44E"},
	{"smiley emoji", "\U0001F600"},
	{"smiling emoji", "\U0001F60A"},
	{"laughing emoji", "\U0001F602"},
	{"heart emoji", "\u2764\ufe0f"},
	{"fire emoji", "\U0001F525"},
	{"checkmark emoji", "\u2705"},
	{"x emoji", "\u274c"},
	{"star emoji", "\u2b50"},
	{"waving emoji", "\U0001F44B"},
	{"crying emoji", "\U0001F622"},
	{"thinking emoji", "\U0001F914"},
	{"party emoji", "\U0001F389"},
	{"clapping emoji", "\U0001F44F"},
	{"warning emoji", "\u26a0\ufe0f"},
	{"question emoji", "\u2753"},
}

var emojiRes = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(emoji))
	for i, e := range emoji {
		out[i] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(e.phrase) + `\b`)
	}
	return out
}()

// LoadMap reads a {"key": "value"} JSON object. A missing file yields an
// empty map; malformed content is reported so Settings can show it.
func LoadMap(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return map[string]string{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return map[string]string{}, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// SortedKeys returns map keys longest first, then alphabetically, so longer
// phrases win over their prefixes deterministically.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// ApplyCustomWords replaces whole-word, case-insensitive mishearings.
func ApplyCustomWords(text string, words map[string]string) string {
	for _, wrong := range SortedKeys(words) {
		if strings.TrimSpace(wrong) == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(wrong))
		right := words[wrong]
		var b strings.Builder
		last := 0
		for _, loc := range re.FindAllStringIndex(text, -1) {
			before, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
			after, _ := utf8.DecodeRuneInString(text[loc[1]:])
			if (loc[0] > 0 && isWordRune(before)) || (loc[1] < len(text) && isWordRune(after)) {
				continue
			}
			b.WriteString(text[last:loc[0]])
			b.WriteString(right)
			last = loc[1]
		}
		b.WriteString(text[last:])
		text = b.String()
	}
	return text
}

// RemoveFillers drops standalone English filler words.
func RemoveFillers(text string) string { return fillerRe.ReplaceAllString(text, "") }

// ApplyEmoji turns spoken emoji names into glyphs.
func ApplyEmoji(text string) string {
	for i, re := range emojiRes {
		text = re.ReplaceAllLiteralString(text, emoji[i].glyph)
	}
	return text
}

var (
	wordlistOnce sync.Once
	wordlist     map[string]bool
)

// systemWords loads the system English dictionary (words of 3+ letters).
func systemWords() map[string]bool {
	wordlistOnce.Do(func() {
		wordlist = map[string]bool{}
		for _, p := range []string{"/usr/share/dict/words", "/usr/share/dict/american-english"} {
			f, err := os.Open(p)
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if w := strings.ToLower(strings.TrimSpace(sc.Text())); len(w) >= 3 {
					wordlist[w] = true
				}
			}
			f.Close()
			if len(wordlist) > 0 {
				break
			}
		}
	})
	return wordlist
}

// segment splits a joined lowercase run into the fewest known words. Among
// splits with that many words it keeps the one with the largest sum of
// squared piece lengths: one long ordinary word plus a short one
// ("ten second") beats two mid-length pieces where one is an obscure
// dictionary entry ("tense cond"). It returns nil when the run is not a
// clean join of two or more words.
func segment(low string, known func(string) bool) []string {
	n := len(low)
	const inf = 1 << 30
	cnt := make([]int, n+1)
	score := make([]int, n+1)
	next := make([]int, n+1)
	for i := range cnt {
		cnt[i] = inf
	}
	cnt[n] = 0
	for i := n - 1; i >= 0; i-- {
		for j := i + 1; j <= n; j++ {
			if cnt[j] == inf || !known(low[i:j]) {
				continue
			}
			c, s := cnt[j]+1, score[j]+(j-i)*(j-i)
			if c < cnt[i] || (c == cnt[i] && s > score[i]) {
				cnt[i], score[i], next[i] = c, s, j
			}
		}
	}
	if cnt[0] == inf || cnt[0] < 2 {
		return nil
	}
	var pieces []string
	for i := 0; i < n; i = next[i] {
		pieces = append(pieces, low[i:next[i]])
	}
	return pieces
}

// RespaceJoins re-segments runs of letters the streaming model glued
// together ("TENSECOND" -> "ten second"). Only upper-case runs of eight or
// more letters that are not themselves words are considered: that is the
// streaming decoder's failure mode, and mixed-case names and terms are left
// exactly as dictated.
func RespaceJoins(text string, vocab map[string]string) string {
	words := systemWords()
	extra := map[string]bool{}
	for w := range vocab {
		extra[strings.ToLower(w)] = true
	}
	if len(words) == 0 && len(extra) == 0 {
		return text
	}
	known := func(w string) bool { return words[w] || extra[w] }
	var b strings.Builder
	start := -1
	flush := func(end int) {
		run := text[start:end]
		low := strings.ToLower(run)
		if len(run) >= 8 && run == strings.ToUpper(run) && !known(low) {
			if pieces := segment(low, known); pieces != nil {
				b.WriteString(strings.Join(pieces, " "))
				return
			}
		}
		b.WriteString(run)
	}
	for i, r := range text {
		if r < utf8.RuneSelf && unicode.IsLetter(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			flush(i)
			start = -1
		}
		b.WriteRune(r)
	}
	if start >= 0 {
		flush(len(text))
	}
	return b.String()
}

var (
	multiSpace = regexp.MustCompile(` {2,}`)
	doubled    = regexp.MustCompile(`[,;:]([.!?,])`)
)

const clausePunct = ",.;:!?-–—"

// tidyAfterFillers repairs punctuation orphaned by removed fillers: a
// punctuation-only token is dropped at the start, after a word that already
// ends a clause, or when it holds a dangling dash; otherwise it rejoins the
// word before it. "um, so -. we" -> "so we", "Yes um, ok" -> "Yes, ok".
func tidyAfterFillers(text string) string {
	fields := strings.Fields(text)
	out := fields[:0]
	for _, f := range fields {
		if strings.Trim(f, clausePunct) != "" {
			out = append(out, f)
			continue
		}
		if len(out) == 0 || strings.ContainsAny(f, "-–—") {
			continue
		}
		prev := out[len(out)-1]
		if last, _ := utf8.DecodeLastRuneInString(prev); strings.ContainsRune(clausePunct, last) {
			continue
		}
		out[len(out)-1] = prev + f
	}
	text = doubled.ReplaceAllString(strings.Join(out, " "), "$1")
	return strings.TrimLeft(text, clausePunct+" ")
}

// Options selects deterministic cleanup steps.
type Options struct {
	Words  map[string]string
	Filler bool
	Emoji  bool
}

// Cleanup applies custom words, filler removal, spacing repair, then spoken emoji.
func Cleanup(text string, o Options) string {
	if len(o.Words) > 0 {
		text = ApplyCustomWords(text, o.Words)
	}
	if o.Filler {
		text = tidyAfterFillers(RemoveFillers(text))
	}
	text = RespaceJoins(text, o.Words)
	if o.Emoji {
		text = ApplyEmoji(text)
	}
	return strings.TrimSpace(multiSpace.ReplaceAllString(text, " "))
}

// SnippetMatch returns the exact snippet when text starts with a snippet
// keyword followed by at most two more words; longer dictation is never
// replaced by a snippet.
func SnippetMatch(text string, snippets map[string]string) (string, bool) {
	stripped := strings.TrimSpace(text)
	low := strings.ToLower(stripped)
	for _, kw := range SortedKeys(snippets) {
		k := strings.ToLower(kw)
		if k == "" || !strings.HasPrefix(low, k) {
			continue
		}
		rest := strings.TrimSpace(stripped[len(k):])
		if len(strings.Fields(rest)) <= 2 {
			return snippets[kw], true
		}
	}
	return "", false
}
