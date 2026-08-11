package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	ansiReset  = "\033[0m"
	ansiGray   = "\033[90m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
)

// colorHandler 为交互式客户端终端输出提供按日志级别区分的颜色。
type colorHandler struct {
	level  slog.Level
	writer io.Writer
	attrs  []slog.Attr
	mu     *sync.Mutex
}

// NewLogger 创建一个结构化日志器，输出到 stderr。
func NewLogger(level slog.Level) *slog.Logger {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler)
}

// NewColorLogger 创建面向终端的彩色结构化日志器。
func NewColorLogger(level slog.Level) *slog.Logger {
	return slog.New(&colorHandler{
		level:  level,
		writer: os.Stdout,
		mu:     &sync.Mutex{},
	})
}

// Enabled 判断当前日志级别是否需要输出。
func (h *colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle 渲染一条带颜色的终端日志。
func (h *colorHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	attributes := make([]slog.Attr, 0, len(h.attrs)+record.NumAttrs())
	attributes = append(attributes, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attributes = append(attributes, attr)
		return true
	})
	parts := make([]string, 0, len(attributes))
	for _, attr := range attributes {
		value := attr.Value.Resolve()
		parts = append(parts, fmt.Sprintf("%s=%v", attr.Key, value.Any()))
	}
	timestamp := record.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	_, err := fmt.Fprintf(
		h.writer,
		"%s%s%s %s%-5s%s %s%s%s %s\n",
		ansiGray,
		timestamp.Format("15:04:05"),
		ansiReset,
		logLevelColor(record.Level),
		record.Level.String(),
		ansiReset,
		ansiCyan,
		record.Message,
		ansiReset,
		strings.Join(parts, " "),
	)
	return err
}

// WithAttrs 返回带固定属性的新 Handler。
func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

// WithGroup 保持结构化日志接口兼容；终端输出不额外显示分组层级。
func (h *colorHandler) WithGroup(_ string) slog.Handler {
	return h
}

// logLevelColor 返回日志级别对应的 ANSI 前景色。
func logLevelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	case level >= slog.LevelInfo:
		return ansiGreen
	default:
		return ansiCyan
	}
}

// NewFileLogger 创建一个输出到文件的日志器。
func NewFileLogger(path string, level slog.Level) (*slog.Logger, *os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, nil, err
	}
	handler := slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler), f, nil
}
