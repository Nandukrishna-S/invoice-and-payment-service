package httpx

import (
	"context"
	"log/slog"
)

type logAttrsKey struct{}

// WithLogAttrs stores attributes that every later log call made with this
// context (or a child) will include, e.g. request_id, business_id.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// ContextHandler adds the attributes stored by WithLogAttrs to each record.
type ContextHandler struct{ slog.Handler }

func NewContextHandler(next slog.Handler) *ContextHandler { return &ContextHandler{next} }

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(logAttrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs and WithGroup must re-wrap, or derived loggers lose the context behaviour.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{h.Handler.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{h.Handler.WithGroup(name)}
}
