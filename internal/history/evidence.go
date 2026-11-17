package history

import (
	"strings"

	"github.com/cplieger/go/textsafe"
)

// The log tail a run keeps.
const (
	tailLines = 200
	tailBytes = 64 << 10
)

// logTail is a host's log tail as a run keeps it: escape sequences and
// control characters but newlines and tabs gone, at most its last
// tailLines lines and tailBytes bytes.
func logTail(raw []byte) string {
	s := clean(stripEscapes(string(raw)))
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > tailLines {
		s = strings.Join(lines[len(lines)-tailLines:], "")
	}
	return textsafe.CapBytesTail(s, tailBytes)
}

// clean drops a carriage return and turns every other rune textsafe calls
// unsafe, but a tab, into a space.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return -1
		case r == '\t':
			return r
		case textsafe.IsUnsafeMultiLine(r):
			return ' '
		}
		return r
	}, s)
}

// stripEscapes removes ECMA-48 escape sequences: control sequences (ESC [
// or CSI), the control strings OSC, DCS, SOS, PM and APC up to their BEL or
// string terminator, and every other escape with its intermediates and
// final byte (https://ecma-international.org/publications-and-standards/standards/ecma-48/).
// An unterminated sequence runs to the end.
func stripEscapes(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b && i+1 < len(rs) && rs[i+1] == '[', r == 0x9b:
			if r == 0x1b {
				i++
			}
			i = skipControlSequence(rs, i+1)
		case r == 0x1b && i+1 < len(rs) && strings.ContainsRune("]PX^_", rs[i+1]), strings.ContainsRune("\u009d\u0090\u0098\u009e\u009f", r):
			if r == 0x1b {
				i++
			}
			i = skipControlString(rs, i+1)
		case r == 0x1b:
			i = skipEscape(rs, i+1)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// skipControlSequence answers the index of a control sequence's final
// byte, its parameters and intermediates starting at i.
func skipControlSequence(rs []rune, i int) int {
	for ; i < len(rs); i++ {
		if rs[i] >= 0x40 && rs[i] <= 0x7e {
			return i
		}
		if rs[i] < 0x20 || rs[i] > 0x3f {
			return i - 1
		}
	}
	return len(rs)
}

// skipControlString answers the index of the last rune of a control
// string's terminator: BEL, ESC \ or ST.
func skipControlString(rs []rune, i int) int {
	for ; i < len(rs); i++ {
		switch {
		case rs[i] == 0x07, rs[i] == 0x9c:
			return i
		case rs[i] == 0x1b && i+1 < len(rs) && rs[i+1] == '\\':
			return i + 1
		}
	}
	return len(rs)
}

// skipEscape answers the index of an escape's final byte, its
// intermediates starting at i.
func skipEscape(rs []rune, i int) int {
	for i < len(rs) && rs[i] >= 0x20 && rs[i] <= 0x2f {
		i++
	}
	if i < len(rs) && rs[i] >= 0x30 && rs[i] <= 0x7e {
		return i
	}
	return i - 1
}
