package logging

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
	r.addEntry(time.Now(), level, message)
}

func (r *RingBuffer) addEntry(ts time.Time, level, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ts.IsZero() {
		ts = time.Now()
	}
	entry := Entry{
		Time:    ts.Format("2006-01-02 15:04:05"),
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

type ringBufferHandler struct {
	buffer *RingBuffer
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

func newRingBufferHandler(buffer *RingBuffer, level slog.Leveler) slog.Handler {
	return &ringBufferHandler{buffer: buffer, level: level}
}

func (h *ringBufferHandler) Enabled(_ context.Context, level slog.Level) bool {
	if h.level == nil {
		return true
	}
	return level >= h.level.Level()
}

func (h *ringBufferHandler) Handle(_ context.Context, rec slog.Record) error {
	if h.buffer == nil {
		return nil
	}
	msg := h.formatMessage(rec)
	h.buffer.addEntry(rec.Time, rec.Level.String(), msg)
	return nil
}

func (h *ringBufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	copied := h.clone()
	copied.attrs = append(copied.attrs, attrs...)
	return copied
}

func (h *ringBufferHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	copied := h.clone()
	copied.groups = append(copied.groups, name)
	return copied
}

func (h *ringBufferHandler) clone() *ringBufferHandler {
	copied := &ringBufferHandler{
		buffer: h.buffer,
		level:  h.level,
		attrs:  make([]slog.Attr, len(h.attrs)),
		groups: make([]string, len(h.groups)),
	}
	copy(copied.attrs, h.attrs)
	copy(copied.groups, h.groups)
	return copied
}

func (h *ringBufferHandler) formatMessage(rec slog.Record) string {
	base := trimNewline(rec.Message)
	parts := make([]string, 0, len(h.attrs)+4)
	groupPrefix := strings.Join(h.groups, ".")

	for _, a := range h.attrs {
		appendAttrParts(&parts, groupPrefix, a)
	}
	rec.Attrs(func(a slog.Attr) bool {
		appendAttrParts(&parts, groupPrefix, a)
		return true
	})
	if len(parts) == 0 {
		return base
	}
	if base == "" {
		return strings.Join(parts, " ")
	}
	return base + " | " + strings.Join(parts, " ")
}

func appendAttrParts(parts *[]string, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}

	key := joinAttrKey(prefix, attr.Key)
	if attr.Value.Kind() == slog.KindGroup {
		nextPrefix := key
		for _, ga := range attr.Value.Group() {
			appendAttrParts(parts, nextPrefix, ga)
		}
		return
	}

	if key == "" {
		return
	}
	*parts = append(*parts, key+"="+fmt.Sprint(attr.Value.Any()))
}

func joinAttrKey(prefix, key string) string {
	switch {
	case prefix == "":
		return key
	case key == "":
		return prefix
	default:
		return prefix + "." + key
	}
}

type multiHandler struct {
	handlers []slog.Handler
}

func newMultiHandler(handlers ...slog.Handler) slog.Handler {
	filtered := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	return &multiHandler{handlers: filtered}
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *multiHandler) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, handler := range h.handlers {
		if !handler.Enabled(ctx, rec.Level) {
			continue
		}
		if err := handler.Handle(ctx, rec); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		next = append(next, handler.WithAttrs(attrs))
	}
	return &multiHandler{handlers: next}
}

func (h *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(h.handlers))
	for _, handler := range h.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return &multiHandler{handlers: next}
}

func Setup(logFile string, buffer *RingBuffer) (*slog.Logger, error) {
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	fileHandler := slog.NewTextHandler(f, opts)
	stdoutHandler := slog.NewTextHandler(os.Stdout, opts)
	bufferHandler := newRingBufferHandler(buffer, opts.Level)

	logger := slog.New(newMultiHandler(fileHandler, stdoutHandler, bufferHandler))
	slog.SetDefault(logger)
	return logger, nil
}
