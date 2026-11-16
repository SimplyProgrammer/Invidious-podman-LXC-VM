package adaptive

import (
	"io"
	"strings"
	"testing"
)

func TestFailureCaptureBodyCapturesWithoutChangingStream(t *testing.T) {
	original := strings.Repeat("é", 1200)
	var captured string
	body := newFailureCaptureBody(io.NopCloser(strings.NewReader(original)), func(value string) {
		captured = value
	})
	read, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading wrapped body: %v", err)
	}
	if string(read) != original {
		t.Fatal("wrapped body changed the response stream")
	}
	if got := len([]rune(captured)); got != 1024 {
		t.Fatalf("captured %d characters, want 1024", got)
	}
	if captured != strings.Repeat("é", 1024) {
		t.Fatal("captured response does not contain the expected prefix")
	}
}

func TestTruncateFailureDiagnostic(t *testing.T) {
	input := strings.Repeat("x", 1100)
	got := truncateFailureDiagnostic(input)
	if len([]rune(got)) != 1024 {
		t.Fatalf("got %d characters, want 1024", len([]rune(got)))
	}
	if got != strings.Repeat("x", 1024) {
		t.Fatal("transport diagnostic was not truncated to the expected prefix")
	}
}
