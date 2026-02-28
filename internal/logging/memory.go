package logging

import (
	"log/slog"
	"os"
	"sync"
	"time"
)

type Entry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type RingBuffer struct {
	mu      sync.RWMutex
	maxSize int
	items   []Entry
}

func NewRingBuffer(maxSize int) *RingBuffer {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &RingBuffer{maxSize: maxSize, items: make([]Entry, 0, maxSize)}
}

func (r *RingBuffer) Add(level, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := Entry{
		Time:    time.Now().Format("2006-01-02 15:04:05"),
		Level:   level,
		Message: message,
	}
	r.items = append(r.items, entry)
	if len(r.items) > r.maxSize {
		r.items = r.items[len(r.items)-r.maxSize:]
	}
}

func (r *RingBuffer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = r.items[:0]
}

func (r *RingBuffer) Query(level, search string, start, limit int) ([]Entry, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if start < 0 {
		start = 0
	}
	if limit <= 0 {
		limit = 100
	}

	filtered := make([]Entry, 0, len(r.items))
	for _, e := range r.items {
		if level != "" && level != "ALL" && e.Level != level {
			continue
		}
		if search != "" {
			if !containsFold(e.Message, search) && !containsFold(e.Level, search) && !containsFold(e.Time, search) {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	total := len(filtered)
	if start >= total {
		return []Entry{}, total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return filtered[start:end], total
}

func containsFold(s, sub string) bool {
	return len(sub) == 0 || slogStringContainsFold(s, sub)
}

func slogStringContainsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca := a[i]
		cb := b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

type BufferWriter struct {
	buffer *RingBuffer
	level  string
}

func NewBufferWriter(buffer *RingBuffer, level string) *BufferWriter {
	return &BufferWriter{buffer: buffer, level: level}
}

func (w *BufferWriter) Write(p []byte) (n int, err error) {
	msg := string(p)
	w.buffer.Add(w.level, trimNewline(msg))
	return len(p), nil
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func Setup(logFile string, buffer *RingBuffer) (*slog.Logger, error) {
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	handler := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}
