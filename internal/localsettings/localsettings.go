package localsettings

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
)

type Loads struct {
	Extensions, Skins []string
}

type Settings struct {
	ExtensionDirectory, StyleDirectory, Server, ScriptPath, DefaultSkin string
}

const (
	blockStart = "// >>> phuo (managed block, edit phuo.json instead)"
	blockEnd   = "// <<< phuo"
	gap        = `(?:\s|//[^\n]*|#.*|/\*[\s\S]*?\*/)*`
	names      = `((?:['"][^'"]*['"]` + gap + `,?` + gap + `)+)`
)

var (
	loadRe = regexp.MustCompile(`wfLoad(Extension|Skin)s?` + gap + `\(` + gap +
		`(?:\[` + gap + names + gap + `\]` +
		`|array` + gap + `\(` + gap + names + gap + `\)` +
		`|` + names + `)` + gap + `\)`)
	nameRe    = regexp.MustCompile(`['"]([^'"]+)['"]`)
	settingRe = regexp.MustCompile(
		`\$wg(ExtensionDirectory|StyleDirectory|Server|ScriptPath|DefaultSkin)\s*=\s*(?:'([^'$]*)'|"([^"$]*)")\s*;`,
	)
)

type File struct {
	Outside  Loads // active loads the user wrote; phuo never duplicates or touches them
	InBlock  Loads
	Disabled Loads // commented-out user loads; phuo treats them as deliberately off
	Settings
	Line map[string]int // key -> line of its user-written load (active, else commented)
	raw  []byte
}

func (l Loads) Has(key string) bool {
	typ, name, _ := strings.Cut(key, "/")
	if typ == "skins" {
		return slices.Contains(l.Skins, name)
	}
	return slices.Contains(l.Extensions, name)
}

func (l Loads) Keys() []string {
	keys := make([]string, 0, len(l.Extensions)+len(l.Skins))
	for _, n := range l.Extensions {
		keys = append(keys, "extensions/"+n)
	}
	for _, n := range l.Skins {
		keys = append(keys, "skins/"+n)
	}
	return keys
}

func (l *Loads) Add(key string) {
	typ, name, _ := strings.Cut(key, "/")
	if typ == "skins" {
		l.Skins = append(l.Skins, name)
	} else {
		l.Extensions = append(l.Extensions, name)
	}
}

func Scan(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	f, err := ScanBytes(b)
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func ScanBytes(b []byte) (File, error) {
	start, end, err := blockRange(b)
	if err != nil {
		return File{}, err
	}
	cm, err := comments(b)
	if err != nil {
		return File{}, err
	}
	f := File{Line: map[string]int{}}
	const (
		disabled = 1 << iota
		outside
		inBlock
	)
	state := map[string]int{}
	var order []string
	line, at := 1, 0
	for _, m := range loadRe.FindAllSubmatchIndex(b, -1) {
		inside := start >= 0 && m[0] >= start && m[1] <= end
		off := commented(cm, m[0])
		if inside && off {
			continue
		}
		for at < m[0] {
			if b[at] == '\n' {
				line++
			}
			at++
		}
		bit := outside
		switch {
		case inside:
			bit = inBlock
		case off:
			bit = disabled
		}
		typ := "extensions/"
		if b[m[2]] == 'S' {
			typ = "skins/"
		}
		var lo, hi int
		for _, i := range []int{4, 6, 8} {
			if m[i] >= 0 {
				lo, hi = m[i], m[i+1]
				break
			}
		}
		for _, name := range loadNames(b, lo, hi, cm, !off) {
			key := typ + name
			if state[key] == 0 {
				order = append(order, key)
			}
			if bit != inBlock && state[key]&outside == 0 && (bit == outside || state[key]&disabled == 0) {
				f.Line[key] = line
			}
			state[key] |= bit
		}
	}
	for _, key := range order {
		switch s := state[key]; {
		case s&outside != 0:
			f.Outside.Add(key)
		case s&inBlock != 0:
			f.InBlock.Add(key)
		default:
			f.Disabled.Add(key)
		}
	}
	s := &f.Settings
	for _, m := range settingRe.FindAllSubmatchIndex(b, -1) {
		if commented(cm, m[0]) {
			continue
		}
		v := m[4:6]
		if v[0] < 0 {
			v = m[6:8]
		}
		val := string(b[v[0]:v[1]])
		switch string(b[m[2]:m[3]]) {
		case "ExtensionDirectory":
			s.ExtensionDirectory = val
		case "StyleDirectory":
			s.StyleDirectory = val
		case "Server":
			s.Server = val
		case "ScriptPath":
			s.ScriptPath = val
		case "DefaultSkin":
			s.DefaultSkin = val
		}
	}
	f.raw = b
	return f, nil
}

func comments(b []byte) (r [][2]int, err error) {
	lineEnd := func(i int) int {
		if j := bytes.IndexByte(b[i:], '\n'); j >= 0 {
			return i + j
		}
		return len(b)
	}
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '\'' || c == '"':
			for i++; i < len(b) && b[i] != c; i++ {
				if b[i] == '\\' {
					i++
				}
			}
		// see https://php.watch/versions/8.0/attributes#syntax
		case c == '#' && (i+1 == len(b) || b[i+1] != '['), c == '/' && i+1 < len(b) && b[i+1] == '/':
			e := lineEnd(i)
			r = append(r, [2]int{i, e})
			i = e
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			j := bytes.Index(b[i+2:], []byte("*/"))
			if j < 0 {
				return nil, fmt.Errorf("unterminated /* comment on line %d", 1+bytes.Count(b[:i], []byte("\n")))
			}
			e := i + 2 + j + 2
			r = append(r, [2]int{i, e})
			i = e - 1
		}
	}
	return r, nil
}

func Write(path string, want Loads) (changed bool, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return write(path, b, want)
}

func (f File) Write(path string, want Loads) (bool, error) {
	return write(path, f.raw, want)
}

func write(path string, b []byte, want Loads) (bool, error) {
	out, err := Render(b, want)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if bytes.Equal(out, b) {
		return false, nil
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func Render(b []byte, want Loads) ([]byte, error) {
	crlf := bytes.Contains(b, []byte("\r\n"))
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	exts := slices.Clone(want.Extensions)
	skins := slices.Clone(want.Skins)
	slices.Sort(exts)
	slices.Sort(skins)

	var block string
	if len(exts) > 0 || len(skins) > 0 {
		var lines []string
		lines = append(lines, blockStart)
		if len(exts) > 0 {
			lines = append(lines, "wfLoadExtensions( [ "+quoteList(exts)+" ] );")
		}
		if len(skins) > 0 {
			lines = append(lines, "wfLoadSkins( [ "+quoteList(skins)+" ] );")
		}
		lines = append(lines, blockEnd)
		block = strings.Join(lines, nl) + nl
	}

	start, end, err := blockRange(b)
	if err != nil {
		return nil, err
	}
	if _, err := comments(b); err != nil {
		return nil, err
	}
	switch {
	case start < 0 && block == "":
		return b, nil
	case start < 0:
		out := slices.Clone(b)
		// a trailing lone '\r' counts as a line end
		// appending '\n' would turn it into CRLF and flip crlf next run
		if len(out) > 0 && out[len(out)-1] != '\n' && out[len(out)-1] != '\r' {
			out = append(out, nl...)
		}
		return append(out, block...), nil
	case block == "":
		return removeBlock(b, start, end, crlf), nil
	}
	return slices.Concat(b[:start], []byte(block), b[end:]), nil
}

// calls inside the managed block, and commented-out calls, are deliberate
func Drop(b []byte, keys []string) ([]byte, error) {
	if len(keys) == 0 {
		return b, nil
	}
	drop := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		drop[k] = struct{}{}
	}
	start, end, err := blockRange(b)
	if err != nil {
		return nil, err
	}
	cm, err := comments(b)
	if err != nil {
		return nil, err
	}
	matches := loadRe.FindAllSubmatchIndex(b, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		if commented(cm, m[0]) || (start >= 0 && m[0] >= start && m[1] <= end) {
			continue
		}
		blob := -1
		for _, g := range []int{4, 6, 8} {
			if m[g] >= 0 {
				blob = g
				break
			}
		}
		if blob < 0 {
			continue
		}
		typ := "extensions/"
		if b[m[2]] == 'S' {
			typ = "skins/"
		}
		var kept []string
		hit := false
		for _, name := range loadNames(b, m[blob], m[blob+1], cm, true) {
			if _, ok := drop[typ+name]; ok {
				hit = true
				continue
			}
			kept = append(kept, name)
		}
		if !hit {
			continue
		}
		if len(kept) == 0 {
			b = cutCall(b, m[0], m[1])
			continue
		}
		b = slices.Concat(b[:m[blob]], []byte(quoteList(kept)), b[m[blob+1]:])
	}
	return b, nil
}

func loadNames(b []byte, lo, hi int, cm [][2]int, skipCommented bool) []string {
	var names []string
	for _, n := range nameRe.FindAllSubmatchIndex(b[lo:hi], -1) {
		if skipCommented && commented(cm, lo+n[0]) {
			continue
		}
		names = append(names, string(b[lo+n[2]:lo+n[3]]))
	}
	return names
}

func commented(cm [][2]int, p int) bool {
	_, ok := slices.BinarySearchFunc(cm, p, func(r [2]int, p int) int {
		switch {
		case r[1] <= p:
			return -1
		case r[0] > p:
			return 1
		}
		return 0
	})
	return ok
}

func cutCall(b []byte, from, to int) []byte {
	end := to
	for end < len(b) && (b[end] == ' ' || b[end] == '\t') {
		end++
	}
	if end < len(b) && b[end] == ';' {
		end++
	}
	ls := bytes.LastIndexByte(b[:from], '\n') + 1
	le := len(b)
	if i := bytes.IndexByte(b[end:], '\n'); i >= 0 {
		le = end + i + 1
	}
	onlySpace := true
	for _, c := range b[ls:from] {
		if c != ' ' && c != '\t' && c != '\r' {
			onlySpace = false
			break
		}
	}
	if onlySpace {
		for _, c := range b[end:le] {
			if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				onlySpace = false
				break
			}
		}
	}
	if onlySpace {
		return slices.Concat(b[:ls], b[le:])
	}
	return slices.Concat(b[:from], b[end:])
}

func quoteList(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = "'" + n + "'"
	}
	return strings.Join(parts, ", ")
}

// a half-deleted or duplicated block is an error
// guessing its extent could swallow the user's own settings
func blockRange(b []byte) (start, end int, err error) {
	start = bytes.Index(b, []byte(blockStart))
	if start < 0 {
		return -1, -1, nil
	}
	rel := bytes.Index(b[start:], []byte(blockEnd))
	if rel < 0 {
		return 0, 0, errors.New("phuo block has no closing " + blockEnd + " line")
	}
	end = start + rel + len(blockEnd)
	if bytes.Contains(b[start+len(blockStart):], []byte(blockStart)) {
		return 0, 0, errors.New("more than one phuo block")
	}
	if end < len(b) && b[end] == '\r' {
		end++
	}
	if end < len(b) && b[end] == '\n' {
		end++
	}
	return start, end, nil
}

func removeBlock(b []byte, start, end int, crlf bool) []byte {
	// eat one surrounding blank line so removal doesn't leave a double gap
	from, to := start, end
	if from > 0 && (b[from-1] == '\n' || b[from-1] == '\r') {
		from--
		if from > 0 && b[from] == '\n' && b[from-1] == '\r' {
			from--
		}
	}
	out := append([]byte(nil), b[:from]...)
	rest := b[to:]
	if len(out) > 0 && len(rest) > 0 {
		if crlf {
			out = append(out, '\r', '\n')
		} else {
			out = append(out, '\n')
		}
		for len(rest) > 0 && (rest[0] == '\n' || rest[0] == '\r') {
			rest = rest[1:]
		}
	}
	out = append(out, rest...)
	return out
}
