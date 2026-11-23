package client_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/ocpi/client"
)

// Retrying cannot help only when the partner refused the request itself.
func TestPermanent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bad request", &client.Error{HTTPStatus: 400}, true},
		{"unauthorized", &client.Error{HTTPStatus: 401}, true},
		{"not found", &client.Error{HTTPStatus: 404}, true},
		{"request timeout", &client.Error{HTTPStatus: 408}, false},
		{"too many requests", &client.Error{HTTPStatus: 429}, false},
		{"server error", &client.Error{HTTPStatus: 500}, false},
		{"OCPI error on a 200", &client.Error{HTTPStatus: 200, StatusCode: 2001}, false},
		{"wrapped", fmt.Errorf("posting: %w", &client.Error{HTTPStatus: 403}), true},
		{"no answer", errors.New("connection refused"), false},
	}
	for _, tc := range tests {
		if got := client.Permanent(tc.err); got != tc.want {
			t.Errorf("%s: Permanent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  error
		want string
	}{
		{&client.Error{HTTPStatus: 500}, "HTTP 500"},
		{&client.Error{HTTPStatus: 200, StatusCode: 2003}, "HTTP 200, OCPI status_code 2003"},
		{errors.New("refused"), "no answer"},
	} {
		if got := client.Describe(tc.err); got != tc.want {
			t.Errorf("Describe(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
