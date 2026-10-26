package obs

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	in := `Get "https://api.telegram.org/bot123456789:AAHfoo_bar-baz1234567890/getMe": sk-abcdef1234567890abcd`
	out := Redact(in)
	if strings.Contains(out, "AAHfoo_bar") {
		t.Fatalf("bot token survived redaction: %q", out)
	}
	if strings.Contains(out, "abcdef1234567890abcd") {
		t.Fatalf("api key survived redaction: %q", out)
	}
	if !strings.Contains(out, "bot<redacted>") || !strings.Contains(out, "sk-<redacted>") {
		t.Fatalf("placeholders missing: %q", out)
	}
}

func TestNewRedactingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf)

	line := "Get \"https://api.telegram.org/bot123456789:AAHfoo_bar-baz1234567890/getMe\"" +
		": retry with sk-abcdef1234567890abcd\n"
	n, err := w.Write([]byte(line))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// The count is the caller's byte count, not the redacted one's: io.Writer
	// must report len(p) when it returns no error, and callers that check the
	// count treat anything smaller as a short write.
	if n != len(line) {
		t.Fatalf("Write returned %d, want %d (len of input)", n, len(line))
	}
	if got := buf.String(); strings.Contains(got, "AAHfoo_bar") {
		t.Fatalf("bot token survived the redacting writer: %q", got)
	}
	if got := buf.String(); strings.Contains(got, "abcdef1234567890abcd") {
		t.Fatalf("api key survived the redacting writer: %q", got)
	}
	if got := buf.String(); !strings.Contains(got, "bot<redacted>") || !strings.Contains(got, "sk-<redacted>") {
		t.Fatalf("placeholders missing: %q", got)
	}
}

// TestRedactingWriterThroughStdLogger pins the case the writer exists for: the
// stdlib log package, whose output is otherwise never scrubbed.
func TestRedactingWriterThroughStdLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(NewRedactingWriter(&buf), "", 0)
	logger.Printf("upstream rejected bot123456789:AAHfoo_bar-baz1234567890 (sk-abcdef1234567890abcd)")

	got := buf.String()
	if strings.Contains(got, "AAHfoo_bar") || strings.Contains(got, "abcdef1234567890abcd") {
		t.Fatalf("stdlib logger leaked a credential: %q", got)
	}
	if !strings.Contains(got, "bot<redacted>") || !strings.Contains(got, "sk-<redacted>") {
		t.Fatalf("placeholders missing: %q", got)
	}
}

func TestLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Errorf("upstream said %s", "bot123456789:AAHfoo_bar-baz1234567890/getMe")
	if strings.Contains(buf.String(), "AAHfoo_bar") {
		t.Fatalf("logger leaked a token: %q", buf.String())
	}
	if !strings.HasPrefix(buf.String(), "ERROR ") {
		t.Fatalf("level prefix missing: %q", buf.String())
	}
}

// Provider keys with hyphens and bare bot tokens appear in real error strings;
// both must be redacted, not just the two shapes that happened to be tested.
func TestRedactCoversHyphenatedKeysAndBareTokens(t *testing.T) {
	cases := []string{
		"openai said: sk-proj-abcDEF1234567890xyz",
		"anthropic said: sk-ant-api03-abcdefghijklmnop",
		"telegram said 123456789:AAHfoo_bar-baz1234567890QRS",
	}
	for _, in := range cases {
		out := Redact(in)
		if strings.Contains(out, "abcDEF1234567890xyz") ||
			strings.Contains(out, "abcdefghijklmnop") ||
			strings.Contains(out, "AAHfoo_bar-baz1234567890QRS") {
			t.Fatalf("credential survived redaction: %q", out)
		}
	}
}
