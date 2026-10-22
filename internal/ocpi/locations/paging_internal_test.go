package locations

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPagingOverSeveralLocations is the test hook the issue allows: config
// has one Location, so the source is swapped for several to see the Link
// header and every page boundary.
func TestPagingOverSeveralLocations(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	var all []Location
	for i := range 5 {
		all = append(all, Location{ID: "L" + strconv.Itoa(i), LastUpdated: DateTime(base.Add(time.Duration(i) * time.Hour))})
	}
	const moduleURL = "http://cpms.example/ocpi/2.3.0/locations"
	s := &Sender{url: moduleURL, all: func() []Location { return all }}

	get := func(query string) (ids []string, h http.Header) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.serveList(rec, httptest.NewRequest(http.MethodGet, "/x"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d", query, rec.Code)
		}
		var env struct {
			Data []struct{ ID string } `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		for _, l := range env.Data {
			ids = append(ids, l.ID)
		}
		return ids, rec.Header()
	}

	// Follow the Link headers from the first page to the last.
	from := url.QueryEscape("2026-10-01T09:00:00Z")
	query := "?limit=2&date_from=" + from
	var seen []string
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("Link headers do not end")
		}
		ids, h := get(query)
		seen = append(seen, ids...)
		if got := h.Get("X-Total-Count"); got != "4" {
			t.Errorf("%s: X-Total-Count = %q, want 4 (L1-L4 match date_from)", query, got)
		}
		if got := h.Get("X-Limit"); got != "2" {
			t.Errorf("%s: X-Limit = %q, want 2", query, got)
		}
		link := h.Get("Link")
		if link == "" {
			break
		}
		next, ok := strings.CutPrefix(link, "<"+moduleURL+"?")
		if !ok || !strings.HasSuffix(next, `>; rel="next"`) {
			t.Fatalf("Link = %q", link)
		}
		q, err := url.ParseQuery(strings.TrimSuffix(next, `>; rel="next"`))
		if err != nil {
			t.Fatal(err)
		}
		if q.Get("date_from") != "2026-10-01T09:00:00Z" {
			t.Errorf("Link %q drops the date_from filter", link)
		}
		query = "?" + q.Encode()
	}
	if got := strings.Join(seen, ","); got != "L1,L2,L3,L4" {
		t.Errorf("pages gave %s, want L1,L2,L3,L4 once each, oldest first", got)
	}

	// A page that ends exactly at the last object has no Link.
	if _, h := get("?offset=3&limit=2"); h.Get("Link") != "" {
		t.Errorf("last page has Link %q", h.Get("Link"))
	}
	if _, h := get("?offset=2&limit=2"); !strings.Contains(h.Get("Link"), "offset=4") {
		t.Errorf("Link = %q, want the next page at offset=4", h.Get("Link"))
	}
	// A huge offset is an empty page, not an overflow.
	if ids, _ := get("?offset=9223372036854775807"); len(ids) != 0 {
		t.Errorf("huge offset gave %v", ids)
	}
}
