package control

import (
	"fmt"
	"strings"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// ResolveCharger picks the charger a command addresses. An empty id means "the
// only one", which is unambiguous with one charger configured and an
// ErrChargerRequired otherwise, listing the ids to choose from. A name that is
// not configured is ErrUnknownCharger. The CLI calls it too, so a typo needs
// no running cpms to be reported.
func ResolveCharger(cfg *config.Config, id string) (config.Charger, error) {
	if id == "" {
		if len(cfg.Chargers) == 1 {
			return cfg.Chargers[0], nil
		}
		return config.Charger{}, fmt.Errorf("%w with %d chargers configured; one of: %s",
			ErrChargerRequired, len(cfg.Chargers), ChargerIDs(cfg))
	}
	ch, ok := cfg.ChargerByID(id)
	if !ok {
		return config.Charger{}, fmt.Errorf("%w %q; configured: %s", ErrUnknownCharger, id, ChargerIDs(cfg))
	}
	return ch, nil
}

// ChargerIDs lists the configured charger ids for an error message.
func ChargerIDs(cfg *config.Config) string {
	ids := make([]string, len(cfg.Chargers))
	for i, ch := range cfg.Chargers {
		ids[i] = ch.ID
	}
	return strings.Join(ids, ", ")
}

// chargerFor is the charger a command goes to: the EVSE's, if it names one
// (and chargerID, if also given, must agree), otherwise chargerID resolved by
// ResolveCharger.
func (s *Service) chargerFor(evseUID, chargerID string) (string, error) {
	if evseUID == "" {
		ch, err := ResolveCharger(s.cfg, chargerID)
		return ch.ID, err
	}
	e, ok := s.cfg.EVSEByUID(evseUID)
	if !ok {
		return "", fmt.Errorf("%w %q", ocpp.ErrUnknownEVSE, evseUID)
	}
	if chargerID != "" && chargerID != e.Charger {
		return "", fmt.Errorf("%w: EVSE %s is on charger %s, not %s", ErrChargerMismatch, evseUID, e.Charger, chargerID)
	}
	return e.Charger, nil
}

// chargePoint is the command interface of the charger a command goes to.
func (s *Service) chargePoint(evseUID, chargerID string) (ocpp.ChargePoint, error) {
	id, err := s.chargerFor(evseUID, chargerID)
	if err != nil {
		return nil, err
	}
	return s.commands.Command(id)
}
