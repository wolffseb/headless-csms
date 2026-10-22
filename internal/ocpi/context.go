package ocpi

import "context"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	correlationIDKey
)

// WithRequestID returns ctx carrying an X-Request-ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithCorrelationID returns ctx carrying an X-Correlation-ID. The OCPI client
// shares it, so an outbound call can continue the correlation of the request
// that caused it.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// RequestID is the X-Request-ID of the request ctx belongs to: the one the
// peer sent, or the one the server generated. It is empty outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// CorrelationID is the X-Correlation-ID of the request ctx belongs to: the one
// the peer sent, or the one the server generated. It is empty outside a
// request.
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}
