package ui

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

type Options struct {
	JSON       bool
	Silent     bool
	Verbose    bool
	NoColor    bool
	NoProgress bool
	Out        io.Writer
	Err        io.Writer
}

type Reporter struct {
	out, errw io.Writer
	color     bool
	progress  bool
	silent    bool
	verbose   bool
	json      bool
	mu        sync.Mutex
	spinIdx   int
	haveProg  bool
	ticking   bool
	progMsg   string
}

var spinner = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

const Accent = "#ffff75"

func New(opts Options) *Reporter {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	errw := opts.Err
	if errw == nil {
		errw = os.Stderr
	}
	tty := false
	if f, ok := out.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
		if tty {
			enableVT(f)
		}
	}
	noColor := opts.NoColor || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
	color := tty && !noColor && !opts.JSON
	progress := tty && !opts.NoProgress && !opts.JSON && !opts.Silent
	return &Reporter{
		out: out, errw: errw,
		color: color, progress: progress,
		silent: opts.Silent, verbose: opts.Verbose, json: opts.JSON,
	}
}

func (r *Reporter) Info(format string, args ...any) {
	if r.silent || r.json {
		return
	}
	r.println(r.out, "", format, args...)
}

func (r *Reporter) Warn(format string, args ...any) {
	if r.silent {
		return
	}
	prefix := "warn: "
	if r.color {
		prefix = "\x1b[33mwarn:\x1b[0m "
	}
	r.println(r.errw, prefix, format, args...)
}

func (r *Reporter) Error(format string, args ...any) {
	prefix := "error: "
	if r.color {
		prefix = "\x1b[31merror:\x1b[0m "
	}
	r.println(r.errw, prefix, format, args...)
}

func (r *Reporter) Step(format string, args ...any) {
	if r.silent || r.json {
		return
	}
	prefix := "✓ "
	if r.color {
		prefix = "\x1b[32m✓\x1b[0m "
	}
	r.println(r.out, prefix, format, args...)
}

func (r *Reporter) Progress(format string, args ...any) {
	if !r.progress {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.progMsg = fmt.Sprintf(format, args...)
	r.drawProgress()
	if !r.ticking {
		r.ticking = true
		go r.tick()
	}
}

func (r *Reporter) tick() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		r.mu.Lock()
		if !r.ticking {
			r.mu.Unlock()
			return
		}
		r.drawProgress()
		r.mu.Unlock()
	}
}

// caller holds r.mu
func (r *Reporter) drawProgress() {
	ch := spinner[r.spinIdx%len(spinner)]
	r.spinIdx++
	fmt.Fprintf(r.errw, "\r\x1b[K%c %s", ch, r.progMsg)
	r.haveProg = true
}

func (r *Reporter) ClearProgress() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ticking = false
	r.progMsg = ""
	if r.haveProg {
		fmt.Fprint(r.errw, "\r\x1b[K")
		r.haveProg = false
	}
}

func (r *Reporter) println(w io.Writer, prefix, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.haveProg {
		fmt.Fprint(r.errw, "\r\x1b[K")
		r.haveProg = false
	}
	fmt.Fprint(w, prefix)
	fmt.Fprintf(w, format, args...)
	if !strings.HasSuffix(format, "\n") {
		fmt.Fprintln(w)
	}
}

func Width() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

type Table struct {
	Header  []string
	Rows    [][]string
	Unicode bool
	Width int
}

func (t Table) Render(w io.Writer) error {
	cols := len(t.Header)
	for _, row := range t.Rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for i, h := range t.Header {
		widths[i] = utf8.RuneCountInString(h)
	}
	limit := t.Width
	if limit == 0 {
		limit = termWidth(w)
	}
	var floor []int
	if limit > 0 {
		floor = slices.Clone(widths)
	}
	for _, row := range t.Rows {
		for i, cell := range row {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	if floor != nil {
		fitWidths(widths, floor, limit)
	}
	ellipsis := "..."
	if t.Unicode {
		ellipsis = "…"
	}
	el := utf8.RuneCountInString(ellipsis)
	var hsep, vsep, tl, tr, bl, br, lj, rj, tj, bj, cj string
	if t.Unicode {
		hsep, vsep = "─", "│"
		tl, tr, bl, br = "┌", "┐", "└", "┘"
		lj, rj, tj, bj, cj = "├", "┤", "┬", "┴", "┼"
	} else {
		hsep, vsep = "-", "|"
		tl, tr, bl, br = "+", "+", "+", "+"
		lj, rj, tj, bj, cj = "+", "+", "+", "+", "+"
	}
	line := func(left, mid, right string) string {
		var b strings.Builder
		b.WriteString(left)
		for i, w := range widths {
			if i > 0 {
				b.WriteString(mid)
			}
			b.WriteString(strings.Repeat(hsep, w+2))
		}
		b.WriteString(right)
		return b.String()
	}
	row := func(cells []string) string {
		var b strings.Builder
		b.WriteString(vsep)
		for i := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			n := utf8.RuneCountInString(cell)
			if n > widths[i] {
				cell = cut(cell, widths[i], ellipsis, el)
				n = widths[i]
			}
			b.WriteByte(' ')
			b.WriteString(cell)
			for range widths[i] - n {
				b.WriteByte(' ')
			}
			b.WriteByte(' ')
			b.WriteString(vsep)
		}
		return b.String()
	}
	if _, err := fmt.Fprintln(w, line(tl, tj, tr)); err != nil {
		return err
	}
	if len(t.Header) > 0 {
		if _, err := fmt.Fprintln(w, row(t.Header)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, line(lj, cj, rj)); err != nil {
			return err
		}
	}
	for _, r := range t.Rows {
		if _, err := fmt.Fprintln(w, row(r)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, line(bl, bj, br))
	return err
}

// a line as wide as the terminal sits on the pending-wrap column
// and the newline then prints a blank row
func termWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	n, _, err := term.GetSize(int(f.Fd()))
	if err != nil || n <= 1 {
		return 0
	}
	return n - 1
}

func fitWidths(widths, floor []int, limit int) {
	budget := limit - 3*len(widths) - 1
	if len(widths) == 0 || budget < 0 {
		return
	}
	if shrink(widths, floor, budget) > budget {
		clear(floor)
		shrink(widths, floor, budget)
	}
}

func shrink(widths, floor []int, budget int) int {
	sum, hi := 0, 0
	for _, w := range widths {
		sum += w
		hi = max(hi, w)
	}
	if sum <= budget {
		return sum
	}
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if capped(widths, floor, mid) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	sum = capped(widths, floor, lo)
	slack := budget - sum
	for i, w := range widths {
		lim := max(lo, floor[i])
		if w <= lim {
			continue
		}
		w = lim
		// a header floor is already above the cap
		// thus the leftover goes to a capped column
		if lim == lo && slack > 0 {
			w++
			slack--
		}
		widths[i] = w
	}
	return budget - slack
}

func capped(widths, floor []int, cap int) int {
	sum := 0
	for i, w := range widths {
		sum += min(w, max(cap, floor[i]))
	}
	return sum
}

func cut(s string, n int, ellipsis string, el int) string {
	keep := n
	if el < n {
		keep = n - el
	} else {
		ellipsis = ""
	}
	i := 0
	for range keep {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + ellipsis
}
