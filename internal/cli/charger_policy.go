package cli

import (
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// chargerPolicy is how `cpms run` tells the OCPP server which stations may
// connect and which timeouts each one gets. It lives apart from run so a test
// can exercise the same closures run installs.
type chargerPolicy struct {
	cfg *config.Config
}

// accept admits only configured stations; anything else dialling in is a
// misconfiguration worth rejecting loudly.
func (p chargerPolicy) accept(id string) bool {
	_, ok := p.cfg.ChargerByID(id)
	return ok
}

// callTimeout is the charger's own call_timeout, 0 (use the server default)
// for an unknown id.
func (p chargerPolicy) callTimeout(id string) time.Duration {
	ch, _ := p.cfg.ChargerByID(id)
	return ch.CallTimeout.Duration()
}

// idleTimeout is the charger's own heartbeat_timeout, 0 (use the server
// default) for an unknown id.
func (p chargerPolicy) idleTimeout(id string) time.Duration {
	ch, _ := p.cfg.ChargerByID(id)
	return ch.HeartbeatTimeout.Duration()
}
