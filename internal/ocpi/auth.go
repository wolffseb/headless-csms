package ocpi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

// Access is what a credentials token lets its holder do. The levels are
// ordered: a route that needs AccessRegistration is also open to
// AccessModules, because a registered party still reaches versions and its
// own credentials with TOKEN_C.
type Access int

const (
	// AccessNone is an unknown token, or none at all.
	AccessNone Access = iota
	// AccessRegistration is TOKEN_A before registration: versions, version
	// details and credentials, and nothing else.
	AccessRegistration
	// AccessModules is TOKEN_C after registration: every endpoint.
	AccessModules
)

// String names the level for logs and test failures.
func (a Access) String() string {
	switch a {
	case AccessNone:
		return "none"
	case AccessRegistration:
		return "registration"
	case AccessModules:
		return "modules"
	default:
		return "unknown"
	}
}

// Authenticator decides what a credentials token grants. It sees the decoded
// token, never the header, and is consulted on every request, so an
// implementation backed by registration state takes effect the moment that
// state changes.
type Authenticator interface {
	Access(token string) Access
}

// AuthenticatorFunc adapts a plain function to Authenticator, which is the
// short way to stub one in a test.
type AuthenticatorFunc func(token string) Access

// Access calls f.
func (f AuthenticatorFunc) Access(token string) Access { return f(token) }

// NewStaticAuthenticator grants registration access to tokenA and nothing to
// any other token. It is the stand-in until the credentials module knows
// whether we are registered.
func NewStaticAuthenticator(tokenA string) Authenticator {
	a := NewSecret(tokenA)
	return AuthenticatorFunc(func(token string) Access {
		if a.Matches(token) {
			return AccessRegistration
		}
		return AccessNone
	})
}

// Secret is an expected credentials token, kept only as a digest so that
// checking a presented token takes the same time wherever they differ.
// Comparing digests rather than the tokens themselves also hides the length
// of the expected token, which subtle.ConstantTimeCompare alone would leak.
type Secret struct {
	sum [sha256.Size]byte
}

// NewSecret holds token for later comparison.
func NewSecret(token string) Secret {
	return Secret{sum: sha256.Sum256([]byte(token))}
}

// Matches reports whether token is the one s was made from, in constant time.
func (s Secret) Matches(token string) bool {
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], s.sum[:]) == 1
}

// tokenFromRequest extracts the credentials token from
// "Authorization: Token <base64>". It returns false for a missing header, a
// different scheme, or anything that is not strict standard Base64 (RFC 4648
// §4, padded). A raw, unencoded token is refused rather than tried as a
// fallback: that is how pre-2.2-d2 peers behave, and accepting it here would
// hide exactly that bug in a counterparty.
func tokenFromRequest(r *http.Request) (string, bool) {
	scheme, encoded, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	// Auth schemes are case-insensitive (RFC 9110 §11.1).
	if !ok || !strings.EqualFold(scheme, "Token") {
		return "", false
	}
	token, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(token) == 0 {
		return "", false
	}
	return string(token), true
}
