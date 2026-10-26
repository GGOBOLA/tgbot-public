// Package obs provides structured, redacting logs. Credential-shaped text is
// stripped before it reaches any writer, so a token cannot reach the journal
// through an error string.
package obs

import (
	"fmt"
	"io"
	"regexp"
	"sync"
)

var (
	// With or without the "bot" prefix the library and Telegram both use, and
	// with the hyphen/underscore alphabet provider keys actually use: the plain
	// `sk-[A-Za-z0-9]` pattern let `sk-proj-…`, `sk-ant-…` and a bare
	// `123456:AA…` (embedded in an upstream JSON error) through untouched.
	botTokenPattern = regexp.MustCompile(`(?:bot)?[0-9]{6,12}:[A-Za-z0-9_-]{20,}`)
	apiKeyPattern   = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)
)

// Redact replaces credential-shaped substrings with a placeholder.
func Redact(s string) string {
	s = botTokenPattern.ReplaceAllString(s, "bot<redacted>")
	return apiKeyPattern.ReplaceAllString(s, "sk-<redacted>")
}

// RedactingWriter applies Redact to every Write, for code that logs through the
// standard library's log package rather than through Logger. Wrapping the
// process output once covers every log.Printf call site; no call site has to
// change.
//
// It holds no lock of its own: log.Logger serialises calls to its output, which
// is the intended caller. A credential split across two Write calls is not
// caught -- safe there, because log.Logger hands over one complete line per
// call.
type RedactingWriter struct {
	w io.Writer
}

// NewRedactingWriter returns a writer that redacts each write before passing it
// to w.
func NewRedactingWriter(w io.Writer) io.Writer { return &RedactingWriter{w: w} }

// Write redacts p and forwards it. It reports len(p), not the redacted length:
// io.Writer's contract requires the caller's byte count when the error is nil,
// and a caller that checks the count (fmt, bufio, io.Copy) treats a smaller one
// as a short write.
func (r *RedactingWriter) Write(p []byte) (int, error) {
	redacted := []byte(Redact(string(p)))
	n, err := r.w.Write(redacted)
	if err != nil {
		return 0, err
	}
	// A wrapped writer that accepts only part of the line and reports no error
	// breaks io.Writer's contract; passing that through as success would tell
	// the logger a truncated line was delivered.
	if n < len(redacted) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// Logger writes redacted lines to one writer.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a Logger writing to w.
func New(w io.Writer) *Logger { return &Logger{w: w} }

func (l *Logger) logf(level, format string, args ...any) {
	if l == nil || l.w == nil {
		return
	}
	line := Redact(fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	// A logger has nowhere to report a failed write; the write is best effort.
	_, _ = fmt.Fprintf(l.w, "%s %s\n", level, line)
}

// Infof logs an informational line.
func (l *Logger) Infof(format string, args ...any) { l.logf("INFO", format, args...) }

// Warnf logs a warning line.
func (l *Logger) Warnf(format string, args ...any) { l.logf("WARN", format, args...) }

// Errorf logs an error line.
func (l *Logger) Errorf(format string, args ...any) { l.logf("ERROR", format, args...) }
