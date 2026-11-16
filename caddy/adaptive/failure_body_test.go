package adaptive

import (
	"io"
	"strings"
	"testing"
)

func TestFailureCaptureBodyCapturesWithoutChangingStream(t *testing.T) {
	original := strings.Repeat("é", 600)
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
	if got := len([]rune(captured)); got != 512 {
		t.Fatalf("captured %d characters, want 512", got)
	}
	if captured != strings.Repeat("é", 512) {
		t.Fatal("captured response does not contain the expected prefix")
	}
}
