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

// TagAuthorised reports whether a station may start a session for tag, which
// is what the 1.6 handler answers Authorize and StartTransaction with. OCPP
// idTags compare case-insensitively (§7.15). A tag is accepted if it is:
//
//   - one of the configured tags (auth.default_id_tag, auth.id_tags);
//   - the tag of an active reservation, so that it can be redeemed;
//   - the tag of a START_SESSION within TagGrantWindow of the command.
func (s *Service) TagAuthorised(tag string) bool {
	if s.cfg != nil && s.cfg.AcceptsIDTag(tag) {
		return true
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.reservations {
		if r.Status == ReservationActive && strings.EqualFold(r.IDTag, tag) {
			return true
		}
	}
	return s.tagGrants[strings.ToLower(tag)].After(s.now())
}
