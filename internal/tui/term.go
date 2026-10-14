// Package tui is the terminal screen for run and pair: plain ANSI and golang.org/x/term, no UI framework, to keep
// the binary and the memory small.
package tui

import (
	"bufio"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/term"
)

// IsTerminal reports whether stdin and stdout are a terminal: the only case where auto mode draws a screen.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

var errNoTerminal = errors.New("для экрана нужен терминал; запустите с --ui=plain")

type screen struct {
	out   io.Writer
	keys  chan rune
	state *term.State
}

// openScreen switches to raw mode and the alternate screen; close restores both.
func openScreen() (*screen, error) {
	st, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return nil, errNoTerminal
	}
	s := &screen{out: os.Stdout, keys: make(chan rune, 8), state: st}
	io.WriteString(s.out, "\x1b[?1049h\x1b[?25l")
	go s.readKeys(bufio.NewReader(os.Stdin))
	return s, nil
}

func (s *screen) readKeys(r *bufio.Reader) {
	for {
		c, _, err := r.ReadRune()
		if err != nil {
			return
		}
		s.keys <- action(c)
	}
}

func (s *screen) close() {
	io.WriteString(s.out, "\x1b[?25h\x1b[?1049l")
	term.Restore(int(os.Stdin.Fd()), s.state)
}

// draw replaces the frame in place: raw mode needs \r\n, and \x1b[K clears what a longer old line left behind.
func (s *screen) draw(frame string) {
	var b strings.Builder
	b.WriteString("\x1b[H")
	b.WriteString(strings.ReplaceAll(strings.TrimRight(frame, "\n"), "\n", "\x1b[K\r\n"))
	b.WriteString("\x1b[K\x1b[J")
	io.WriteString(s.out, b.String())
}

// action folds case and the Russian layout onto the Latin action keys; Ctrl-C quits as in a normal terminal.
func action(r rune) rune {
	switch r = unicode.ToLower(r); r {
	case 3, 'й':
		return 'q'
	case 'ы':
		return 's'
	case 'г':
		return 'u'
	case 'з':
		return 'p'
	case 'н':
		return 'y'
	}
	return r
}

// Ring keeps the last log lines for the screen; it is the logger's writer while a screen is shown.
type Ring struct {
	mu    sync.Mutex
	lines []string
}

const ringSize = 20

func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		r.lines = append(r.lines, l)
	}
	if over := len(r.lines) - ringSize; over > 0 {
		r.lines = r.lines[over:]
	}
	return len(p), nil
}

func (r *Ring) last(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines[max(0, len(r.lines)-n):]...)
}

var noColor = os.Getenv("NO_COLOR") != ""

func sgr(code, s string) string {
	if noColor || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return sgr("1", s) }
func dim(s string) string    { return sgr("2", s) }
func red(s string) string    { return sgr("31", s) }
func green(s string) string  { return sgr("32", s) }
func yellow(s string) string { return sgr("33", s) }
func cyan(s string) string   { return sgr("36", s) }
