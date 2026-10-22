// Package client calls the counterparty's OCPI API: the registration calls
// during the credentials handshake, and later the Locations PATCHes and
// command results.
//
// Every call carries the credentials token, a fresh X-Request-ID, and an
// X-Correlation-ID that continues the one of the inbound request that caused
// it, when there is one. Every answer is decoded as an OCPI envelope; a
// non-2xx status or a status_code other than 1000 comes back as an *Error.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// DefaultTimeout bounds a call whose context has no deadline of its own.
const DefaultTimeout = 10 * time.Second

// maxResponseBytes caps what is read of an answer. The counterparty's
// answers to us are small (version lists, empty acknowledgements); anything
// near this is a misdirected URL.
const maxResponseBytes = 4 << 20

// Options configure a Client.
type Options struct {
	// HTTP sends the requests; nil means a client with no timeout of its own,
	// because every call is bounded by its context.
	HTTP *http.Client
	// Token returns the credentials token to send: TOKEN_B, read fresh on
	// every call because a credentials PUT replaces it. WithToken overrides
	// it for the registration calls.
	Token func() string
	// Log gets one debug line per call. It never sees a token.
	Log *slog.Logger
}

// Client calls one counterparty.
type Client struct {
	http  *http.Client
	token func() string
	log   *slog.Logger
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{}
	}
	if opts.Token == nil {
		opts.Token = func() string { return "" }
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Client{http: opts.HTTP, token: opts.Token, log: opts.Log}
}

// WithToken returns a Client that sends token instead of the configured one.
// The credentials handshake uses it: the token B it calls back with is not
// stored until the handshake has succeeded.
func (c *Client) WithToken(token string) *Client {
	cp := *c
	cp.token = func() string { return token }
	return &cp
}

// Response is a decoded OCPI envelope. Data is left raw for the caller to
// decode into the type it expects.
type Response struct {
	HTTPStatus    int
	Data          json.RawMessage
	StatusCode    ocpi.StatusCode
	StatusMessage string
	Timestamp     string
}

// DecodeData decodes the envelope's data into v.
func (r *Response) DecodeData(v any) error {
	if len(r.Data) == 0 {
		return errors.New("ocpi response has no data")
	}
	if err := json.Unmarshal(r.Data, v); err != nil {
		return fmt.Errorf("decoding ocpi response data: %w", err)
	}
	return nil
}

// Error is an answer the counterparty gave that is not a success: a non-2xx
// HTTP status, a status_code other than 1000, or a body that is not an OCPI
// envelope. A call that got no answer at all (unreachable, timed out) returns
// the transport error instead.
type Error struct {
	HTTPStatus int
	// StatusCode is zero when the body was not an envelope.
	StatusCode ocpi.StatusCode
	Message    string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("ocpi: HTTP %d: %s", e.HTTPStatus, e.Message)
	}
	return fmt.Sprintf("ocpi: HTTP %d, status_code %d: %s", e.HTTPStatus, e.StatusCode, e.Message)
}

// Get fetches url.
func (c *Client) Get(ctx context.Context, url string) (*Response, error) {
	return c.Do(ctx, http.MethodGet, url, nil)
}

// Post sends body to url.
func (c *Client) Post(ctx context.Context, url string, body any) (*Response, error) {
	return c.Do(ctx, http.MethodPost, url, body)
}

// Put sends body to url.
func (c *Client) Put(ctx context.Context, url string, body any) (*Response, error) {
	return c.Do(ctx, http.MethodPut, url, body)
}

// Patch sends body to url.
func (c *Client) Patch(ctx context.Context, url string, body any) (*Response, error) {
	return c.Do(ctx, http.MethodPatch, url, body)
}

// Delete calls DELETE on url.
func (c *Client) Delete(ctx context.Context, url string) (*Response, error) {
	return c.Do(ctx, http.MethodDelete, url, nil)
}

// Do sends one request. A nil body sends none; anything else is encoded as
// JSON.
func (c *Client) Do(ctx context.Context, method, url string, body any) (*Response, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("ocpi: encoding %s body: %w", method, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("ocpi: %s %s: %w", method, url, err)
	}

	requestID := ocpi.NewID()
	correlationID := ocpi.CorrelationID(ctx)
	if correlationID == "" {
		correlationID = ocpi.NewID()
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(c.token())))
	req.Header.Set("X-Request-ID", requestID)
	req.Header.Set("X-Correlation-ID", correlationID)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.Debug("ocpi call failed", "method", method, "url", url,
			"request_id", requestID, "correlation_id", correlationID, "error", err)
		return nil, fmt.Errorf("ocpi: %s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("ocpi: %s %s: reading response: %w", method, url, err)
	}
	c.log.Debug("ocpi call", "method", method, "url", url, "status", resp.StatusCode,
		"duration", time.Since(start), "request_id", requestID, "correlation_id", correlationID)

	return decode(resp.StatusCode, raw)
}

// decode turns an answer into a Response, or an *Error for anything that is
// not a success.
func decode(httpStatus int, raw []byte) (*Response, error) {
	if len(raw) > maxResponseBytes {
		return nil, &Error{HTTPStatus: httpStatus, Message: "response body too large"}
	}

	var env struct {
		Data          json.RawMessage `json:"data"`
		StatusCode    *int            `json:"status_code"`
		StatusMessage string          `json:"status_message"`
		Timestamp     string          `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.StatusCode == nil {
		msg := "response is not an OCPI envelope"
		if httpStatus < 200 || httpStatus > 299 {
			msg = http.StatusText(httpStatus)
		}
		return nil, &Error{HTTPStatus: httpStatus, Message: msg}
	}

	code := ocpi.StatusCode(*env.StatusCode)
	if httpStatus < 200 || httpStatus > 299 || code != ocpi.StatusSuccess {
		return nil, &Error{HTTPStatus: httpStatus, StatusCode: code, Message: env.StatusMessage}
	}
	// "null" is what an omitted data looks like once captured raw.
	if string(env.Data) == "null" {
		env.Data = nil
	}
	return &Response{
		HTTPStatus:    httpStatus,
		Data:          env.Data,
		StatusCode:    code,
		StatusMessage: env.StatusMessage,
		Timestamp:     env.Timestamp,
	}, nil
}
