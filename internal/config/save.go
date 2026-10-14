package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gitlab.com/integral-inference/voice-io/internal/paths"
)

// Field describes one editable config key.
type Field struct {
	Section, Key string
	Kind         reflect.Kind
	order        int
}

var schema = func() map[string]map[string]Field {
	out := map[string]map[string]Field{}
	t := reflect.TypeOf(Config{})
	order := 0
	for i := range t.NumField() {
		sf := t.Field(i)
		section := sf.Tag.Get("toml")
		if section == "" || section == "-" || sf.Type.Kind() != reflect.Struct {
			continue
		}
		keys := map[string]Field{}
		for j := range sf.Type.NumField() {
			kf := sf.Type.Field(j)
			key := kf.Tag.Get("toml")
			if key == "" || key == "-" {
				continue
			}
			keys[key] = Field{Section: section, Key: key, Kind: kf.Type.Kind(), order: order}
			order++
		}
		out[section] = keys
	}
	return out
}()

// Updates maps section -> key -> value. Values may be string, bool, int or
// float64 (JSON numbers); each is checked against the schema's field type.
type Updates map[string]map[string]any

// Update persists updates to file (paths.ConfigFile() when empty) and returns
// the resulting configuration. Only requested keys are rewritten; comments,
// unknown sections and formatting elsewhere survive. A malformed existing
// file is never overwritten, and an invalid result leaves the file untouched.
func Update(file string, updates Updates) (*Config, error) {
	if file == "" {
		file = paths.ConfigFile()
	}
	literals, err := literalUpdates(updates)
	if err != nil {
		return nil, err
	}
	var text string
	data, err := os.ReadFile(file)
	switch {
	case err == nil:
		text = string(data)
		if _, perr := Parse(text); perr != nil {
			return nil, fmt.Errorf("existing %s is invalid; fix it before saving: %w", file, perr)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	merged := mergeSections(text, literals)
	cfg, err := Parse(merged)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(file, []byte(merged)); err != nil {
		return nil, err
	}
	return cfg, nil
}

type literalKV struct {
	key, literal string
}

type sectionUpdate struct {
	section string
	values  []literalKV
}

func literalUpdates(updates Updates) ([]sectionUpdate, error) {
	var out []sectionUpdate
	for section, values := range updates {
		fields, ok := schema[section]
		if !ok {
			return nil, fmt.Errorf("unsupported config section: %s", section)
		}
		var kvs []literalKV
		var orders []int
		for key, value := range values {
			field, ok := fields[key]
			if !ok {
				return nil, fmt.Errorf("unsupported config key: %s.%s", section, key)
			}
			lit, err := tomlLiteral(field, value)
			if err != nil {
				return nil, err
			}
			kvs = append(kvs, literalKV{key, lit})
			orders = append(orders, field.order)
		}
		sort.Sort(byOrder{kvs, orders})
		out = append(out, sectionUpdate{section, kvs})
	}
	sort.Slice(out, func(i, j int) bool {
		return sectionOrder(out[i].section) < sectionOrder(out[j].section)
	})
	return out, nil
}

func sectionOrder(section string) int {
	best := math.MaxInt
	for _, f := range schema[section] {
		best = min(best, f.order)
	}
	return best
}

type byOrder struct {
	kvs    []literalKV
	orders []int
}

func (b byOrder) Len() int           { return len(b.kvs) }
func (b byOrder) Less(i, j int) bool { return b.orders[i] < b.orders[j] }
func (b byOrder) Swap(i, j int) {
	b.kvs[i], b.kvs[j] = b.kvs[j], b.kvs[i]
	b.orders[i], b.orders[j] = b.orders[j], b.orders[i]
}

func tomlLiteral(field Field, value any) (string, error) {
	name := field.Section + "." + field.Key
	switch field.Kind {
	case reflect.String:
		s, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%s must be a string", name)
		}
		return tomlString(s), nil
	case reflect.Bool:
		b, ok := value.(bool)
		if !ok {
			return "", fmt.Errorf("%s must be true or false", name)
		}
		return strconv.FormatBool(b), nil
	case reflect.Int:
		var n float64
		switch v := value.(type) {
		case int:
			n = float64(v)
		case int64:
			n = float64(v)
		case float64:
			n = v
		default:
			return "", fmt.Errorf("%s must be a whole number", name)
		}
		if n != math.Trunc(n) || math.IsInf(n, 0) {
			return "", fmt.Errorf("%s must be a whole number", name)
		}
		return strconv.FormatInt(int64(n), 10), nil
	case reflect.Float64:
		var f float64
		switch v := value.(type) {
		case int:
			f = float64(v)
		case int64:
			f = float64(v)
		case float64:
			f = v
		default:
			return "", fmt.Errorf("%s must be a number", name)
		}
		switch {
		case math.IsNaN(f):
			return "nan", nil
		case math.IsInf(f, 1):
			return "inf", nil
		case math.IsInf(f, -1):
			return "-inf", nil
		}
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		return s, nil
	}
	return "", fmt.Errorf("%s cannot be edited", name)
}

// tomlString encodes a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f || r == utf8.RuneError {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

var (
	headingRe = regexp.MustCompile(`^\s*\[([A-Za-z0-9_-]+)\]\s*(?:#.*)?$`)
	keyRe     = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)(\s*=)`)
)

// mergeSections replaces only requested keys, preserving all other text.
// Missing keys are appended to the end of their section; missing sections
// are appended to the end of the file.
func mergeSections(text string, updates []sectionUpdate) string {
	pending := map[string][]literalKV{}
	var order []string
	for _, u := range updates {
		pending[u.section] = append([]literalKV(nil), u.values...)
		order = append(order, u.section)
	}
	var out []string
	section := ""
	inSection := false
	lastEndsNewline := func() bool {
		return len(out) == 0 || strings.HasSuffix(out[len(out)-1], "\n")
	}
	addRemaining := func() {
		if !inSection {
			return
		}
		values, ok := pending[section]
		if !ok {
			return
		}
		delete(pending, section)
		if len(values) > 0 && !lastEndsNewline() {
			out = append(out, "\n")
		}
		for _, kv := range values {
			out = append(out, kv.key+" = "+kv.literal+"\n")
		}
	}
	for _, line := range splitKeepEnds(text) {
		trimmed := strings.TrimRight(line, "\r\n")
		if m := headingRe.FindStringSubmatch(strings.TrimSpace(trimmed)); m != nil {
			addRemaining()
			section, inSection = m[1], true
		}
		if m := keyRe.FindStringSubmatch(line); m != nil && inSection {
			if values, ok := pending[section]; ok {
				if i := indexKV(values, m[2]); i >= 0 {
					out = append(out, m[1]+m[2]+m[3]+" "+values[i].literal+"\n")
					pending[section] = append(values[:i], values[i+1:]...)
					continue
				}
			}
		}
		out = append(out, line)
	}
	addRemaining()
	for _, name := range order {
		values, ok := pending[name]
		if !ok || len(values) == 0 {
			continue
		}
		if !lastEndsNewline() {
			out = append(out, "\n")
		}
		block := "\n[" + name + "]\n"
		if len(out) == 0 {
			block = "[" + name + "]\n"
		}
		out = append(out, block)
		for _, kv := range values {
			out = append(out, kv.key+" = "+kv.literal+"\n")
		}
	}
	return strings.Join(out, "")
}

func indexKV(values []literalKV, key string) int {
	for i, kv := range values {
		if kv.key == key {
			return i
		}
	}
	return -1
}

func splitKeepEnds(text string) []string {
	var lines []string
	for len(text) > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

// writeAtomic writes data to file with mode 0600 via a synced temp file.
func writeAtomic(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}

// Fields lists editable keys for a section, in schema order.
func Fields(section string) []Field {
	var out []Field
	for _, f := range schema[section] {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
	return out
}
