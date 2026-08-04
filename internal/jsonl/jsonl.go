// Package jsonl holds the file-reading helpers shared by the transcript-based
// adapters (Claude Code and Codex).
//
// These lived as near-identical copies in each adapter. That is a real hazard
// rather than mere untidiness: ReadLine encodes how an oversized line is
// handled, and a copy that drifts reintroduces a silent under-report in one
// provider but not the other.
package jsonl

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ReaderBufBytes is the working buffer. Lines longer than this are read in
	// chunks rather than rejected.
	ReaderBufBytes = 256 * 1024

	// MaxLineBytes caps how much of a single line is retained. The largest
	// line observed across 300 live transcripts was ~1.4 MB, so this is ~11x
	// headroom; past it the line is skipped and the caller flags the reading
	// incomplete.
	MaxLineBytes = 16 * 1024 * 1024
)

// ReadLine reads one newline-terminated line of any length, reporting whether
// it had to discard an oversized one.
//
// bufio.Scanner cannot do this. On a line past its cap it halts the entire
// scan, and because Scan() simply returns false, an unchecked Err() makes that
// indistinguishable from a clean EOF — the caller happily reports a partial
// file as a complete session. In a spend monitor that is a bypass: one huge
// tool-result line freezes a session's reported cost for the rest of its life.
// Here the oversized line is consumed and skipped, and parsing continues.
func ReadLine(r *bufio.Reader, max int) (line []byte, truncated bool, err error) {
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return line, truncated, err
		}
		if len(line)+len(chunk) <= max {
			line = append(line, chunk...)
		} else {
			truncated = true
		}
		if !isPrefix {
			return line, truncated, nil
		}
	}
}

// ExpandHome resolves a leading ~ against the user's home directory.
func ExpandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// FirstLine returns the first line of s, trimmed. Used to turn a multi-line
// prompt into a one-line title.
func FirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
