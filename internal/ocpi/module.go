// Package ocpi is the transport every OCPI 2.3.0 module shares: the HTTP
// server, the response envelope, the "Authorization: Token" scheme, the
// X-Request-ID/X-Correlation-ID headers, and the versions endpoints.
//
// It knows nothing about any one module. A module describes itself as a
// Module (identifier, role, routes, the access it needs) and registers with
// the Server, which then serves its routes and lists it in version details.
// Who a token belongs to is the Authenticator's business, so the
// registration-aware one can replace the static one without this package
// changing.
package ocpi

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Version is the one OCPI version we speak.
const Version = "2.3.0"

// Role is an OCPI InterfaceRole: which side of a module an endpoint is.
type Role string

const (
	// RoleSender is the interface of the data owner, which the receiver
	// pulls from (Locations, as a CPO).
	RoleSender Role = "SENDER"
	// RoleReceiver is the interface that gets pushed to (Commands, as a CPO).
	RoleReceiver Role = "RECEIVER"
)

// Module is one OCPI module endpoint as it mounts itself on the Server.
type Module struct {
	// Identifier is the OCPI ModuleID, e.g. "locations".
	Identifier string
	// Role is the interface role listed in version details. For credentials
	// the spec advises SENDER.
	Role Role
	// Path is where the module lives below the version root; empty means the
	// identifier, so "locations" serves {prefix}/2.3.0/locations.
	Path string
	// Access is what every route of the module requires.
	Access Access
	// Routes are the module's handlers.
	Routes []Route
}

// Route is one method and path of a module.
type Route struct {
	// Method is the HTTP method, e.g. http.MethodGet.
	Method string
	// Path is relative to the module's URL and may use ServeMux wildcards:
	// "" is the module URL itself, "{location_id}/{evse_uid}" is below it.
	Path string
	// Handler answers the request. It runs after authentication, with the
	// body capped at MaxBodyBytes and the request id headers already set.
	Handler http.HandlerFunc
}

// identifierRe is what a ModuleID looks like: the spec's lowercase names, and
// custom modules prefixed as it advises ("nltnm-tokens").
var identifierRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func (m *Module) validate() error {
	if !identifierRe.MatchString(m.Identifier) {
		return fmt.Errorf("ocpi: module identifier %q is not a valid ModuleID", m.Identifier)
	}
	if m.Role != RoleSender && m.Role != RoleReceiver {
		return fmt.Errorf("ocpi: module %s: role %q is neither SENDER nor RECEIVER", m.Identifier, m.Role)
	}
	if m.Path == "" {
		m.Path = m.Identifier
	}
	m.Path = strings.Trim(m.Path, "/")
	if m.Path == "" || strings.ContainsAny(m.Path, "{}") {
		return fmt.Errorf("ocpi: module %s: path %q must be a fixed, non-empty path", m.Identifier, m.Path)
	}
	if m.Access != AccessRegistration && m.Access != AccessModules {
		// AccessNone would make the module public, which no OCPI module is.
		return fmt.Errorf("ocpi: module %s: access must be registration or modules, got %s", m.Identifier, m.Access)
	}
	if len(m.Routes) == 0 {
		return fmt.Errorf("ocpi: module %s has no routes", m.Identifier)
	}
	for _, r := range m.Routes {
		if r.Method == "" || r.Handler == nil {
			return fmt.Errorf("ocpi: module %s: route %q needs a method and a handler", m.Identifier, r.Path)
		}
	}
	return nil
}
