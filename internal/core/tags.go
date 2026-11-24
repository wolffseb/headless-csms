package core

import (
	"strings"
	"time"
)

// TagGrantWindow is how long a START_SESSION's tag is accepted at the station:
// the 15 minutes OCPI 2.3.0 allows a CPO to skip real-time authorisation of a
// token for, after the command that carried it.
const TagGrantWindow = 15 * time.Minute

// AuthoriseTag accepts tag at the stations for ttl from now: an OCPI partner
// sent it in a START_SESSION, and the spec has the CPO trust the eMSP to send
// valid tokens. A second grant extends the window, never shortens it.
func (s *Service) AuthoriseTag(tag string, ttl time.Duration) {
	until := s.now().Add(ttl)
	key := strings.ToLower(tag)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, t := range s.tagGrants {
		if !t.After(now) {
			delete(s.tagGrants, k)
		}
	}
	if until.After(s.tagGrants[key]) {
		s.tagGrants[key] = until
	}
}

// TagAuthorised reports whether a station may start a session for tag. It is
// TagAuthorisation without the end.
func (s *Service) TagAuthorised(tag string) bool {
	ok, _ := s.TagAuthorisation(tag)
	return ok
}

// TagAuthorisation reports whether a station may start a session for tag,
// which is what the 1.6 handler answers Authorize and StartTransaction with,
// and until when cpms vouches for it. OCPP idTags compare case-insensitively
// (§7.15). A tag is accepted if it is:
//
//   - one of the configured tags (auth.default_id_tag, auth.id_tags), which
//     has no end: until is the zero time;
//   - the tag of an active reservation, so that it can be redeemed, until the
//     reservation expires (and, while a replacement is out, the original's);
//   - the tag of a START_SESSION within TagGrantWindow of the command, until
//     the window ends.
//
// A station keeps what it is told in its Authorization Cache, so the answer
// must carry the end (idTagInfo.expiryDate), or the entry would never lapse.
// A tag that is vouched for twice has the later end.
func (s *Service) TagAuthorisation(tag string) (ok bool, until time.Time) {
	if s.cfg != nil && s.cfg.AcceptsIDTag(tag) {
		return true, time.Time{}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	vouch := func(end time.Time) {
		ok = true
		if end.After(until) {
			until = end
		}
	}
	for _, r := range s.reservations {
		if r.Status == ReservationActive && strings.EqualFold(r.IDTag, tag) {
			// Accepted until its expiry timer fires, which can be a moment
			// after the expiry itself: the end is then already past, which is
			// what the station should be told.
			vouch(r.ExpiresAt)
		}
	}
	for _, r := range s.reservations {
		// While a replacement is out the station still holds the original.
		if r.Status == ReservationPending && r.prior != nil && strings.EqualFold(r.prior.IDTag, tag) {
			vouch(r.prior.ExpiresAt)
		}
	}
	if end := s.tagGrants[strings.ToLower(tag)]; end.After(s.now()) {
		vouch(end)
	}
	return ok, until
}
