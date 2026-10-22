package ocpi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// StatusCode is an OCPI status code, the four-digit `status_code` of the
// response envelope. It is separate from the HTTP status: a request that
// reaches the OCPI layer answers in this vocabulary.
type StatusCode int

// The status codes of OCPI 2.3.0 (status_codes.asciidoc). Hub errors (4xxx)
// are left out: there is no hub in this setup.
const (
	// StatusSuccess is the generic success code.
	StatusSuccess StatusCode = 1000

	// StatusClientError is the generic client error: the data sent by the
	// client cannot be processed.
	StatusClientError StatusCode = 2000
	// StatusInvalidParameters means invalid or missing parameters, e.g. a
	// missing field or a body that is not valid JSON.
	StatusInvalidParameters StatusCode = 2001
	// StatusNotEnoughInformation means the request carried too little
	// information, e.g. an authorization request without enough detail.
	StatusNotEnoughInformation StatusCode = 2002
	// StatusUnknownLocation means the request named a Location the server
	// does not know, e.g. a START_SESSION command for an unknown location.
	StatusUnknownLocation StatusCode = 2003
	// StatusUnknownToken means the request named a Token the server does not
	// know, e.g. real-time authorization of an unknown token.
	StatusUnknownToken StatusCode = 2004

	// StatusServerError is the generic server error: the request was
	// syntactically correct but could not be processed.
	StatusServerError StatusCode = 3000
	// StatusClientAPIUnusable means we could not use the client's API, e.g.
	// a GET against the counterparty failed during the credentials POST.
	StatusClientAPIUnusable StatusCode = 3001
	// StatusUnsupportedVersion means the parties share no OCPI version.
	StatusUnsupportedVersion StatusCode = 3002
	// StatusNoMatchingEndpoints means the parties have no mutual modules or
	// endpoints, or the expected minimal set is missing, during registration.
	StatusNoMatchingEndpoints StatusCode = 3003
)

// MaxBodyBytes caps every request body. OCPI requests are small objects; a
// larger body is a mistake or an attack, and either way not worth buffering.
const MaxBodyBytes = 1 << 20

// Response is the OCPI response envelope every answer is wrapped in.
type Response struct {
	// Data is omitted when nil: the spec advises leaving it unset where it
	// does not say what goes there, which includes every error.
	Data          any        `json:"data,omitempty"`
	StatusCode    StatusCode `json:"status_code"`
	StatusMessage string     `json:"status_message,omitempty"`
	Timestamp     string     `json:"timestamp"`
}

// FormatDateTime renders t as an OCPI DateTime: RFC 3339 in UTC with a
// literal "Z". OCPI says "+00:00 is not the same as UTC", so the zone is never
// left to time.RFC3339's formatting of a non-UTC location.
func FormatDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// WriteOK answers HTTP 200 with data in a success envelope.
func WriteOK(w http.ResponseWriter, data any) {
	writeEnvelope(w, http.StatusOK, Response{
		Data:          data,
		StatusCode:    StatusSuccess,
		StatusMessage: "Success",
	})
}

// WriteError answers with an HTTP status and an OCPI status code. Use an HTTP
// error status only for failures on the transport layer (auth, unparseable
// JSON, unknown resource); a request that reached the OCPI layer answers 200
// with a 2xxx or 3xxx code.
func WriteError(w http.ResponseWriter, httpStatus int, code StatusCode, message string) {
	writeEnvelope(w, httpStatus, Response{StatusCode: code, StatusMessage: message})
}

func writeEnvelope(w http.ResponseWriter, httpStatus int, resp Response) {
	resp.Timestamp = FormatDateTime(nowFor(w))
	body, err := json.Marshal(resp)
	if err != nil {
		// Only a handler passing unmarshalable data gets here, which is a
		// bug; answer with something the peer can still parse.
		resp = Response{StatusCode: StatusServerError, StatusMessage: "response could not be encoded", Timestamp: resp.Timestamp}
		httpStatus = http.StatusInternalServerError
		body, _ = json.Marshal(resp)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(append(body, '\n'))
}

// nowFor returns the clock of the Server that handed out w, so tests can pin
// timestamps without the helpers growing a clock parameter. A writer from
// anywhere else falls back to the wall clock.
func nowFor(w http.ResponseWriter) time.Time {
	if c, ok := w.(interface{ ocpiNow() time.Time }); ok {
		return c.ocpiNow()
	}
	return time.Now()
}

// DecodeJSON reads the request body into v. On failure it has already
// answered: HTTP 400 with 2001 for a body that is not one JSON value, HTTP 413
// for one over MaxBodyBytes. It returns whether the handler should go on.
//
// Unknown fields are accepted on purpose: OCPI forbids rejecting a payload
// for fields the spec does not document, and peers add vendor fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil {
		// One value, and nothing after it but whitespace.
		switch _, terr := dec.Token(); {
		case terr == io.EOF:
		case terr != nil:
			err = terr
		default:
			err = errors.New("unexpected data after the JSON value")
		}
	}
	if err == nil {
		return true
	}

	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		WriteError(w, http.StatusRequestEntityTooLarge, StatusClientError, "request body too large")
		return false
	}
	WriteError(w, http.StatusBadRequest, StatusInvalidParameters, "request body is not valid JSON")
	return false
}
