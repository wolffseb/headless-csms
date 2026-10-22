package ocpi

import "context"

type ctxKey int

const (
	requestIDKey ctxKey = iota
	correlationIDKey
	tokenKey
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

// NewID returns a fresh X-Request-ID or X-Correlation-ID: a random UUID, the
// form OCPI advises. The outbound client uses it for the ids it originates.
func NewID() string { return newUUID() }

func withToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenKey, token)
}

// TokenFromContext is the credentials token the request carried, which the
// server has already authenticated. A handler whose answer depends on state
// that can change while the request waits (registration does) checks it again
// against that state, because the Authenticator's decision is only as fresh
// as the moment the request arrived. It is a secret: never log it.
func TokenFromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(tokenKey).(string)
	return token, ok
}
