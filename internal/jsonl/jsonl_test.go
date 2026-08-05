package jsonl

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readAll drains r through ReadLine, returning each line and whether any line
// was truncated. It mirrors what the transcript adapters do.
func readAll(t *testing.T, r *bufio.Reader, max int) (lines []string, truncatedAny bool) {
	t.Helper()
	for {
		line, truncated, err := ReadLine(r, max)
		if truncated {
			truncatedAny = true
		}
		if len(line) > 0 {
			lines = append(lines, string(line))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected read error: %v", err)
			}
			return lines, truncatedAny
		}
	}
}

func TestReadLineSplitsOnNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("one\ntwo\nthree\n"))
	lines, truncated := readAll(t, r, MaxLineBytes)

	if truncated {
		t.Error("no line exceeded the cap, but truncated was reported")
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestReadLineKeepsFinalUnterminatedLine(t *testing.T) {
	// Transcripts are appended to live, so the last line is routinely
	// mid-write and has no trailing newline yet. Dropping it would make a
	// session's newest usage record invisible until the next flush.
	r := bufio.NewReader(strings.NewReader("first\nsecond-no-newline"))
	lines, _ := readAll(t, r, MaxLineBytes)

	if len(lines) != 2 || lines[1] != "second-no-newline" {
		t.Fatalf("got %q, want the unterminated final line preserved", lines)
	}
}

func TestReadLineReassemblesLineLongerThanBuffer(t *testing.T) {
	// The isPrefix loop is the whole reason this helper exists. With a
	// buffer far smaller than the line, the line must still come back whole
	// rather than in fragments that each fail to parse as JSON.
	long := strings.Repeat("abcdefgh", 1000) // 8000 bytes
	r := bufio.NewReaderSize(strings.NewReader(long+"\nshort\n"), 16)

	lines, truncated := readAll(t, r, MaxLineBytes)

	if truncated {
		t.Error("line was under the cap; truncated must be false")
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0] != long {
		t.Errorf("long line came back as %d bytes, want %d", len(lines[0]), len(long))
	}
	if lines[1] != "short" {
		t.Errorf("line after the long one = %q, want %q", lines[1], "short")
	}
}

func TestOversizedLineDoesNotEndTheScan(t *testing.T) {
	// This is the property the package comment calls a monitoring bypass.
	// bufio.Scanner halts on an oversized line and reports it as a clean
	// EOF, so every later usage record silently vanishes and the session's
	// cost freezes. ReadLine must skip the line and keep going.
	huge := strings.Repeat("x", 500)
	input := "before\n" + huge + "\nafter\n"
	r := bufio.NewReaderSize(strings.NewReader(input), 16)

	lines, truncated := readAll(t, r, 100)

	if !truncated {
		t.Error("oversized line was not reported as truncated")
	}
	// "after" is the assertion that matters: it lives past the oversized
	// line, so its presence proves the scan continued.
	var sawAfter bool
	for _, l := range lines {
		if l == "after" {
			sawAfter = true
		}
	}
	if !sawAfter {
		t.Fatalf("scan stopped at the oversized line; got %q", lines)
	}
}

func TestOversizedLineIsNotReturnedWhole(t *testing.T) {
	// The retained prefix must stay at or under max. Callers treat a
	// truncated line as unparseable JSON and skip it; returning the full
	// oversized payload would defeat the cap the caller asked for.
	huge := strings.Repeat("x", 5000)
	r := bufio.NewReaderSize(strings.NewReader(huge+"\n"), 16)

	line, truncated, err := ReadLine(r, 100)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(line) > 100 {
		t.Errorf("retained %d bytes, want <= 100", len(line))
	}
}

func TestReadLineStripsCarriageReturn(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("windows\r\nunix\n"))
	lines, _ := readAll(t, r, MaxLineBytes)

	if len(lines) != 2 || lines[0] != "windows" {
		t.Fatalf("got %q, want the CR stripped from the first line", lines)
	}
}

func TestReadLineReportsEOFOnEmptyInput(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))
	line, truncated, err := ReadLine(r, MaxLineBytes)

	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if len(line) != 0 || truncated {
		t.Errorf("got line %q truncated=%v, want empty and false", line, truncated)
	}
}

func TestReadLineHandlesBlankLines(t *testing.T) {
	// A blank line yields a zero-length slice and a nil error, which callers
	// must not mistake for EOF.
	r := bufio.NewReader(strings.NewReader("\ndata\n"))

	line, _, err := ReadLine(r, MaxLineBytes)
	if err != nil {
		t.Fatalf("blank line returned %v, want nil error", err)
	}
	if len(line) != 0 {
		t.Errorf("blank line returned %q, want empty", line)
	}

	line, _, err = ReadLine(r, MaxLineBytes)
	if err != nil || string(line) != "data" {
		t.Errorf("got %q, %v; want \"data\", nil", line, err)
	}
}

func TestMaxLineBytesExceedsObservedTranscripts(t *testing.T) {
	// The cap is documented as ~11x the largest line seen in the wild. If
	// someone lowers it to a value a real transcript can hit, sessions start
	// silently under-reporting.
	const largestObserved = 1_400_000
	if MaxLineBytes < largestObserved*2 {
		t.Errorf("MaxLineBytes = %d, too close to the largest observed line (%d)",
			MaxLineBytes, largestObserved)
	}
	if ReaderBufBytes >= MaxLineBytes {
		t.Error("ReaderBufBytes must stay below MaxLineBytes or the cap never engages")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare tilde", "~", home},
		{"tilde slash", "~/.claude/projects", filepath.Join(home, ".claude/projects")},
		{"absolute path untouched", "/var/log/x", "/var/log/x"},
		{"relative path untouched", "sessions/a.jsonl", "sessions/a.jsonl"},
		{"empty untouched", "", ""},
		// ~otheruser is not expanded: guessing another account's home from
		// the current one would resolve to a path that does not exist.
		{"other user not expanded", "~bob/notes", "~bob/notes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandHome(tt.in); got != tt.want {
				t.Errorf("ExpandHome(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "just a title", "just a title"},
		{"multi line", "the title\nand the body\nmore", "the title"},
		{"crlf", "the title\r\nbody", "the title"},
		{"leading newline", "\nsecond", ""},
		{"trims surrounding space", "  padded title  \nbody", "padded title"},
		{"trims a lone line", "\t spaced \t", "spaced"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstLine(tt.in); got != tt.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
