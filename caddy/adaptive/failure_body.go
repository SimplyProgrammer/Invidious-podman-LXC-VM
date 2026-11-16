package adaptive

import (
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

// failureCaptureBody observes bytes as the existing proxy code reads the body.
// It never reads ahead, so it does not add latency to response streaming.
type failureCaptureBody struct {
	io.ReadCloser
	mu     sync.Mutex
	buf    []byte
	update func(string)
}

func newFailureCaptureBody(body io.ReadCloser, update func(string)) io.ReadCloser {
	return &failureCaptureBody{ReadCloser: body, update: update}
}

func (b *failureCaptureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		readN := n
		b.mu.Lock()
		if len(b.buf) < 2048 {
			remaining := 2048 - len(b.buf)
			if readN > remaining {
				readN = remaining
			}
			b.buf = append(b.buf, p[:readN]...)
		}
		text := strings.ToValidUTF8(string(b.buf), "�")
		if utf8.RuneCountInString(text) > 512 {
			runes := []rune(text)
			text = string(runes[:512])
		}
		update := b.update
		b.mu.Unlock()
		if update != nil {
			update(text)
		}
	}
	return n, err
}
