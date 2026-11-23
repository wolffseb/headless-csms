package client

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"
)

// Backoff bounds. The delay before retry n is baseBackoff·2^(n-1), plus up to
// a quarter of that as jitter, and never more than maxBackoff.
const (
	baseBackoff = time.Second
	maxBackoff  = 30 * time.Second
)

// Backoff is the wait before retry n (1 for the first): 1s, 2s, 4s… capped at
// 30s, with up to 25% jitter so that several calls failing together do not
// retry in lockstep. Everything cpms sends to the partner and retries (status
// pushes, command results) waits this way.
func Backoff(retry int) time.Duration {
	d := baseBackoff
	for i := 1; i < retry && d < maxBackoff; i++ {
		d *= 2
	}
	d += time.Duration(rand.Int64N(int64(d)/4 + 1))
	return min(d, maxBackoff)
}

// Permanent reports whether retrying cannot help: the partner refused the
// request itself. 408 and 429 are 4xx that mean "later", and everything that
// is not an answer (timeouts, refused connections) or not a success envelope
// is retried.
func Permanent(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	s := e.HTTPStatus
	return s >= 400 && s < 500 && s != http.StatusRequestTimeout && s != http.StatusTooManyRequests
}

// Describe names the last HTTP status or OCPI code of a failed call, for an
// error log.
func Describe(err error) string {
	var e *Error
	if errors.As(err, &e) {
		if e.StatusCode != 0 {
			return fmt.Sprintf("HTTP %d, OCPI status_code %d", e.HTTPStatus, e.StatusCode)
		}
		return fmt.Sprintf("HTTP %d", e.HTTPStatus)
	}
	return "no answer"
}
