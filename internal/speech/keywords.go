package speech

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Keyword labels emitted by the spotter.
const (
	Wake = "WAKE"
	Send = "SEND"
)

var phraseWord = regexp.MustCompile(`^[A-Za-z]+(?:['-][A-Za-z]+)*$`)

// KeywordLines compiles the wake and end phrases into the spotter's keyword
// format ("<phones> @WAKE\n<phones> @SEND\n") using the model's English
// lexicon (en.phone). Only words present in the lexicon are accepted.
func KeywordLines(modelDir, wakePhrase, endPhrase string) (string, error) {
	lexicon := filepath.Join(modelDir, "en.phone")
	words := func(phrase, label string) ([]string, error) {
		parts := strings.Fields(phrase)
		if len(parts) == 0 {
			return nil, fmt.Errorf("%s phrase must contain English words", strings.ToLower(label))
		}
		for i, w := range parts {
			if !phraseWord.MatchString(w) {
				return nil, fmt.Errorf("%s phrase %q contains invalid words; "+
					"use space-separated English words from en.phone", strings.ToLower(label), phrase)
			}
			parts[i] = strings.ToUpper(w)
		}
		return parts, nil
	}
	wakeWords, err := words(wakePhrase, Wake)
	if err != nil {
		return "", err
	}
	endWords, err := words(endPhrase, Send)
	if err != nil {
		return "", err
	}
	wanted := map[string]bool{}
	for _, w := range append(append([]string{}, wakeWords...), endWords...) {
		wanted[w] = true
	}
	entries := map[string]string{}
	f, err := os.Open(lexicon)
	if err != nil {
		return "", fmt.Errorf("keyword lexicon unavailable: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		word, pron, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok || !wanted[word] || pron == "" {
			continue
		}
		if _, seen := entries[word]; !seen {
			entries[word] = strings.TrimSpace(pron)
			if len(entries) == len(wanted) {
				break
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	phones := func(ws []string, label string) (string, error) {
		var missing []string
		var out []string
		for _, w := range ws {
			p, ok := entries[w]
			if !ok {
				missing = append(missing, strings.ToLower(w))
				continue
			}
			out = append(out, p)
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("%s phrase has unsupported word(s): %s; choose words in %s",
				strings.ToLower(label), strings.Join(missing, ", "), lexicon)
		}
		return strings.Join(out, " "), nil
	}
	wake, err := phones(wakeWords, Wake)
	if err != nil {
		return "", err
	}
	end, err := phones(endWords, Send)
	if err != nil {
		return "", err
	}
	if wake == end {
		return "", errors.New("wake and end phrases have the same pronunciation; choose different phrases")
	}
	return wake + " @" + Wake + "\n" + end + " @" + Send + "\n", nil
}
