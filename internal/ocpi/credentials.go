package ocpi

import "net/http"

// CredentialsStub is the credentials module before the handshake exists: it
// is listed in version details, as every OCPI implementation must list it,
// and answers HTTP 501 to every method until the real module replaces it.
func CredentialsStub() Module {
	notYet := func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, http.StatusNotImplemented, StatusServerError, "credentials registration is not implemented yet")
	}
	routes := make([]Route, 0, 4)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		routes = append(routes, Route{Method: m, Handler: notYet})
	}
	return Module{
		Identifier: "credentials",
		// The role has no function for credentials; the spec advises SENDER.
		Role:   RoleSender,
		Access: AccessRegistration,
		Routes: routes,
	}
}
