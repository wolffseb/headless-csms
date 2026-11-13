package locations

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// MaxLimit is the most Locations one page returns, and the page size when the
// client names none. X-Limit reports it, or the client's smaller limit.
const MaxLimit = 100

// Snapshotter is the part of core the module reads. *core.Service satisfies
// it.
type Snapshotter interface {
	Snapshot() core.Snapshot
}

// Options configure a Sender.
type Options struct {
	// Config holds the static Location. Its ModTime stamps the static data's
	// last_updated; zero (a config that did not come from a file) means the
	// time the Sender is built.
	Config *config.Config
	// Core supplies the live EVSE status.
	Core Snapshotter
	// URL is the module's advertised URL, e.g.
	// "http://192.168.1.10:8080/ocpi/2.3.0/locations". Link headers point
	// there, so a client follows the same address it was given.
	URL string
}

// Sender serves the Locations Sender interface.
type Sender struct {
	url string
	// all returns every Location, oldest first. There is one, built per
	// request so its status is current; tests replace it to page over more.
	all func() []Location
}

// NewSender builds the module. The config file's time is read here, once:
// config is not reloaded, so there is nothing to stat per request.
func NewSender(opts Options) (*Sender, error) {
	if opts.Config == nil || opts.Core == nil {
		return nil, errors.New("locations: Config and Core are required")
	}
	if opts.URL == "" {
		return nil, errors.New("locations: URL is required")
	}
	staticAt := opts.Config.ModTime
	if staticAt.IsZero() {
		staticAt = time.Now()
	}
	cfg, svc := opts.Config, opts.Core
	return &Sender{
		url: opts.URL,
		all: func() []Location { return []Location{Build(cfg, staticAt, svc.Snapshot())} },
	}, nil
}

// Module is the Sender as it mounts on the OCPI server. Every route needs
// module access: TOKEN_C, once the counterparty has registered.
func (s *Sender) Module() ocpi.Module {
	return ocpi.Module{
		Identifier: "locations",
		Role:       ocpi.RoleSender,
		Access:     ocpi.AccessModules,
		Routes: []ocpi.Route{
			{Method: http.MethodGet, Handler: s.serveList},
			{Method: http.MethodGet, Path: "{location_id}", Handler: s.serveLocation},
			{Method: http.MethodGet, Path: "{location_id}/{evse_uid}", Handler: s.serveEVSE},
			{Method: http.MethodGet, Path: "{location_id}/{evse_uid}/{connector_id}", Handler: s.serveConnector},
		},
	}
}

// page is a parsed GET List query.
type page struct {
	offset, limit    int
	dateFrom, dateTo time.Time
	// raw keeps the filters as the client wrote them, for the Link header.
	rawFrom, rawTo string
}

func parsePage(q url.Values) (page, error) {
	p := page{limit: MaxLimit}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return p, fmt.Errorf("offset %q must be a whole number of 0 or more", v)
		}
		p.offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		// A limit of 0 would make a "next" link that never advances.
		if err != nil || n < 1 {
			return p, fmt.Errorf("limit %q must be a whole number of 1 or more", v)
		}
		p.limit = min(n, MaxLimit)
	}
	var err error
	if p.rawFrom = q.Get("date_from"); p.rawFrom != "" {
		if p.dateFrom, err = parseDateTime(p.rawFrom); err != nil {
			return p, fmt.Errorf("date_from %q is not an OCPI DateTime", p.rawFrom)
		}
	}
	if p.rawTo = q.Get("date_to"); p.rawTo != "" {
		if p.dateTo, err = parseDateTime(p.rawTo); err != nil {
			return p, fmt.Errorf("date_to %q is not an OCPI DateTime", p.rawTo)
		}
	}
	return p, nil
}

// parseDateTime reads an OCPI DateTime. OCPI writes UTC with or without a
// "Z" (no zone means UTC) and allows fractional seconds; an explicit offset
// is not OCPI but is unambiguous, so it is accepted too.
func parseDateTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.ParseInLocation("2006-01-02T15:04:05.999999999", s, time.UTC)
}

// matches applies date_from (inclusive) and date_to (exclusive) to the real
// last_updated, not the whole second it is rendered with: a client polling
// with date_from set to its previous request time (fraction included) must
// still get a change made later in that same second. Echoing a rendered
// last_updated back still works: as date_from it includes the object, as
// date_to it excludes it.
func (p page) matches(l Location) bool {
	at := l.LastUpdated.Time()
	if !p.dateFrom.IsZero() && at.Before(p.dateFrom) {
		return false
	}
	if !p.dateTo.IsZero() && !at.Before(p.dateTo) {
		return false
	}
	return true
}

// nextLink is the Link header for the page after this one, keeping the
// client's filters as the spec asks.
func (s *Sender) nextLink(p page, nextOffset int) string {
	q := url.Values{}
	q.Set("offset", strconv.Itoa(nextOffset))
	q.Set("limit", strconv.Itoa(p.limit))
	if p.rawFrom != "" {
		q.Set("date_from", p.rawFrom)
	}
	if p.rawTo != "" {
		q.Set("date_to", p.rawTo)
	}
	return fmt.Sprintf("<%s?%s>; rel=\"next\"", s.url, q.Encode())
}

func (s *Sender) serveList(w http.ResponseWriter, r *http.Request) {
	p, err := parsePage(r.URL.Query())
	if err != nil {
		ocpi.WriteError(w, http.StatusBadRequest, ocpi.StatusInvalidParameters, err.Error())
		return
	}

	var matched []Location
	for _, l := range s.all() {
		if p.matches(l) {
			matched = append(matched, l)
		}
	}
	total := len(matched)
	start := min(p.offset, total)
	end := total
	if p.limit < total-start {
		end = start + p.limit
	}
	// A copy, never nil: an empty page is "data": [], not a missing field.
	out := append([]Location{}, matched[start:end]...)

	h := w.Header()
	h.Set("X-Total-Count", strconv.Itoa(total))
	h.Set("X-Limit", strconv.Itoa(p.limit))
	if end < total {
		h.Set("Link", s.nextLink(p, end))
	}
	ocpi.WriteOK(w, out)
}

// find resolves the path segments that are present, answering 404 with 2003
// for the first that names nothing. OCPI ids are CiStrings, so case does not
// matter.
func (s *Sender) find(w http.ResponseWriter, r *http.Request) (Location, bool) {
	id := r.PathValue("location_id")
	for _, l := range s.all() {
		if strings.EqualFold(l.ID, id) {
			return l, true
		}
	}
	unknown(w, "location", id)
	return Location{}, false
}

func (s *Sender) findEVSE(w http.ResponseWriter, r *http.Request) (EVSE, bool) {
	l, ok := s.find(w, r)
	if !ok {
		return EVSE{}, false
	}
	uid := r.PathValue("evse_uid")
	e, ok := l.evse(uid)
	if !ok {
		unknown(w, "EVSE", uid)
	}
	return e, ok
}

func unknown(w http.ResponseWriter, what, id string) {
	ocpi.WriteError(w, http.StatusNotFound, ocpi.StatusUnknownLocation, fmt.Sprintf("unknown %s %q", what, id))
}

func (s *Sender) serveLocation(w http.ResponseWriter, r *http.Request) {
	if l, ok := s.find(w, r); ok {
		ocpi.WriteOK(w, l)
	}
}

func (s *Sender) serveEVSE(w http.ResponseWriter, r *http.Request) {
	if e, ok := s.findEVSE(w, r); ok {
		ocpi.WriteOK(w, e)
	}
}

func (s *Sender) serveConnector(w http.ResponseWriter, r *http.Request) {
	e, ok := s.findEVSE(w, r)
	if !ok {
		return
	}
	id := r.PathValue("connector_id")
	c, ok := e.connector(id)
	if !ok {
		unknown(w, "connector", id)
		return
	}
	ocpi.WriteOK(w, c)
}
