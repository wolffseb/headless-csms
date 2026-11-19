package push

import (
	"testing"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// The PATCH URL is the partner's endpoint plus four path segments of ours.
// Location ids and EVSE uids are free text (CiString(36)), so each segment
// is escaped: a `/` must not start a new segment, a `#` or `?` must not cut
// the URL short, and a `%` must not be read as an escape. The endpoint may or
// may not end in a slash, and the result has exactly one between them.
func TestPatchURL(t *testing.T) {
	t.Parallel()

	const endpoint = "https://emsp.example/ocpi/2.3.0/locations"
	for _, tc := range []struct {
		name     string
		endpoint string
		location string
		uid      string
		want     string
	}{
		{"plain", endpoint, "SITE-01", "EVSE-1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE-1"},
		{"trailing slash", endpoint + "/", "SITE-01", "EVSE-1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE-1"},
		{"several trailing slashes", endpoint + "//", "SITE-01", "EVSE-1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE-1"},
		{"slash in the uid", endpoint, "SITE-01", "A/1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/A%2F1"},
		{"hash in the uid", endpoint, "SITE-01", "EVSE#1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE%231"},
		{"question mark in the uid", endpoint, "SITE-01", "EVSE?1",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE%3F1"},
		{"percent in the uid", endpoint, "SITE-01", "EVSE%41",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/SITE-01/EVSE%2541"},
		{"all of them in both ids", endpoint + "/", "S/1#?%", "A/1#?%",
			"https://emsp.example/ocpi/2.3.0/locations/DE/EXA/S%2F1%23%3F%25/A%2F1%23%3F%25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{}
			cfg.OCPI.CountryCode = "DE"
			cfg.OCPI.PartyID = "EXA"
			cfg.Location.ID = tc.location
			if got := patchURL(tc.endpoint, cfg, tc.uid); got != tc.want {
				t.Errorf("patchURL(%q, location %q, uid %q)\n got %s\nwant %s", tc.endpoint, tc.location, tc.uid, got, tc.want)
			}
		})
	}
}
