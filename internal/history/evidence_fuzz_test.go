package history

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cplieger/go/textsafe"
)

// A host's log is untrusted: whatever it holds, the kept tail is valid
// UTF-8 with no escape, no carriage return and no unsafe control rune but
// a tab, within its line and byte caps, and a log with none of those is
// kept as its last lines.
func FuzzLogTail_safe(f *testing.F) {
	f.Add([]byte("\x1b[31mred\x1b[0m\r\nok\n"))
	f.Add([]byte("\x1b]8;;https://x\x1b\\link\x1b]8;;\x07\u009b2K\xff\xfe"))
	f.Add([]byte("\x1bP" + strings.Repeat("a\n", 300)))
	f.Add([]byte("plain\ttext\nline two\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		got := logTail(raw)
		if !utf8.ValidString(got) || len(got) > tailBytes || lineCount(got) > tailLines {
			t.Fatalf("logTail(%q) = %q: not valid UTF-8 or past its caps", raw, got)
		}
		for _, r := range got {
			if r == 0x1b || r == '\r' || (r != '\n' && r != '\t' && textsafe.IsUnsafeMultiLine(r)) {
				t.Fatalf("logTail(%q) = %q keeps %U", raw, got, r)
			}
		}
		s := string(raw)
		if utf8.Valid(raw) && !strings.ContainsFunc(s, func(r rune) bool {
			return r == '\r' || (r != '\n' && r != '\t' && textsafe.IsUnsafeMultiLine(r))
		}) && len(s) <= tailBytes && lineCount(s) <= tailLines && got != s {
			t.Fatalf("logTail(%q) = %q, want a clean log kept whole", raw, got)
		}
	})
}

// lineCount counts s's lines, a last one with no newline included.
func lineCount(s string) int {
	return strings.Count(s, "\n") + min(1, len(s)-strings.LastIndexByte(s, '\n')-1)
}
