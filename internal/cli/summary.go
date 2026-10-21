package cli

import (
	"fmt"
	"net"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// printSummary renders the loaded config as the operator needs to see it: the
// facts they have to get right for the station and the counterparty to reach
// us, and the EVSE mapping that links the two addressing schemes.
func printSummary(cmd *cobra.Command, cfg *config.Config, statePath string) {
	out := cmd.OutOrStdout()

	charger := cfg.Charger.ID + "  (OCPP " + cfg.Charger.OCPPVersion + ")"
	if cfg.Charger.IP != "" {
		charger += "  at " + cfg.Charger.IP
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  Charge point\t%s\n", charger)
	fmt.Fprintf(w, "  OCPP listener\t%s\n", cfg.Server.OCPPBind)
	fmt.Fprintf(w, "  Station CSMS URL\t%s\n", csmsURL(cfg))
	fmt.Fprintf(w, "  Heartbeat timeout\t%s\n", cfg.Charger.HeartbeatTimeout)
	fmt.Fprintf(w, "  OCPI listener\t%s\n", cfg.Server.OCPIBind)
	fmt.Fprintf(w, "  OCPI advertised as\t%s\n", cfg.OCPI.PublicBaseURL)
	fmt.Fprintf(w, "  OCPI party\t%s*%s\n", cfg.OCPI.CountryCode, cfg.OCPI.PartyID)
	fmt.Fprintf(w, "  Default RFID tag\t%s\n", cfg.Auth.DefaultIDTag)
	fmt.Fprintf(w, "  Location\t%s\n", locationLine(cfg))
	fmt.Fprintf(w, "  State file\t%s\n", statePath)
	_ = w.Flush()

	fmt.Fprintln(out)

	t := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, "  EVSE\tEVSE ID\tOCPP CONNECTOR\tCONNECTORS")
	for _, e := range cfg.Location.EVSEs {
		fmt.Fprintf(t, "  %s\t%s\t%d\t%s\n", e.UID, e.EVSEID, e.OCPPConnectorID, connectorSummary(e.Connectors))
	}
	_ = t.Flush()
}

// csmsURL is the URL the station itself has to be configured with. Getting
// this wrong is the most common reason a charger never shows up, so the
// summary spells it out rather than leaving it to be assembled by hand.
func csmsURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Server.OCPPBind)
	if err != nil {
		return "ws://" + cfg.Server.OCPPBind + "/ocpp/" + cfg.Charger.ID
	}
	// A wildcard bind does not name a reachable address; say so instead of
	// printing ws://0.0.0.0/... which the operator cannot use as-is.
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "<this-host>"
	}
	return fmt.Sprintf("ws://%s:%s/ocpp/%s", host, port, cfg.Charger.ID)
}

func locationLine(cfg *config.Config) string {
	l := cfg.Location

	s := l.ID
	if l.Name != "" {
		s += fmt.Sprintf(" %q", l.Name)
	}
	if l.City != "" {
		s += ", " + l.City
	}
	s += " (" + l.Country + ")"

	connectors := 0
	for _, e := range l.EVSEs {
		connectors += len(e.Connectors)
	}
	return fmt.Sprintf("%s — %s, %s", s, plural(len(l.EVSEs), "EVSE"), plural(connectors, "connector"))
}

func connectorSummary(connectors []config.Connector) string {
	parts := make([]string, 0, len(connectors))
	for _, c := range connectors {
		parts = append(parts, fmt.Sprintf("%s/%s/%s %dV %dA",
			c.Standard, c.Format, c.PowerType, c.MaxVoltage, c.MaxAmperage))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %s", n, pluralNoun(noun))
}

// pluralNoun is the regular English plural: "capability" becomes
// "capabilities", but "day" stays "days".
func pluralNoun(noun string) string {
	if n := len(noun); n > 1 && noun[n-1] == 'y' && !strings.ContainsRune("aeiou", rune(noun[n-2])) {
		return noun[:n-1] + "ies"
	}
	return noun + "s"
}
