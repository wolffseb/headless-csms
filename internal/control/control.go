// Package control is the one place that acts on the station. The one-shot CLI,
// OCPI Commands and the TUI all go through it, so "allocate an id, record it,
// send the command, interpret the answer" is written once.
//
// It knows no OCPP version: it talks to the station through ocpp.ChargePoint
// and records outcomes in core. Every failure comes back as a typed error
// (see errors.go), because the CLI turns them into exit codes and OCPI into
// command results.
package control

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
	"github.com/wolffseb/cli-cpms/internal/core"
	"github.com/wolffseb/cli-cpms/internal/ocpp"
)

// DefaultReservationLength is how long a reservation holds an EVSE when the
// caller does not say.
const DefaultReservationLength = 15 * time.Minute

// Commander hands out the command interface for a connected charge point.
// *csms.Server satisfies it.
type Commander interface {
	Command(id string) (ocpp.ChargePoint, error)
}

// Options configure a Service.
type Options struct {
	Core     *core.Service
	Commands Commander
	Config   *config.Config
	// Now is the clock used to compute expiry times. It defaults to time.Now;
	// tests pass the same fake clock they give core.
	Now func() time.Time
	Log *slog.Logger
}

// Service performs actions on the configured station.
type Service struct {
	core     *core.Service
	commands Commander
	cfg      *config.Config
	now      func() time.Time
	log      *slog.Logger
}

// New builds a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Service{
		core:     opts.Core,
		commands: opts.Commands,
		cfg:      opts.Config,
		now:      opts.Now,
		log:      opts.Log,
	}
}

// ReserveParams describe a reservation to make.
type ReserveParams struct {
	EVSEUID string
	// IDTag is the RFID tag the EVSE is held for. Empty means the configured
	// auth.default_id_tag.
	IDTag string
	// ExpiresAt is when the reservation ends. If it is zero, ExpiresIn from
	// now is used, and if that is zero too, DefaultReservationLength. OCPI
	// sends an absolute time; the CLI and TUI think in durations.
	ExpiresAt time.Time
	ExpiresIn time.Duration
	// Source is who asked: "cli", "tui" or "ocpi".
	Source string
	// OCPIReservationID and ResponseURL are kept for OCPI Commands, which
	// identifies reservations by its own id and wants the outcome posted
	// back.
	OCPIReservationID string
	ResponseURL       string
}

// Reserve holds an EVSE for a tag.
//
// The reservation is recorded before ReserveNow goes out, and only becomes
// active once the station accepts it. On any other outcome, including a
// timeout, it is ended straight away: a reservation the station may not
// hold must not linger in core.
func (s *Service) Reserve(ctx context.Context, p ReserveParams) (core.Reservation, error) {
	if err := s.checkEVSE(p.EVSEUID); err != nil {
		return core.Reservation{}, err
	}
	cp, err := s.chargePoint()
	if err != nil {
		return core.Reservation{}, err
	}

	expiresAt := p.ExpiresAt
	if expiresAt.IsZero() {
		length := p.ExpiresIn
		if length <= 0 {
			length = DefaultReservationLength
		}
		expiresAt = s.now().Add(length)
	}

	r, err := s.core.BeginReservation(core.Reservation{
		OCPIReservationID: p.OCPIReservationID,
		ChargePointID:     cp.ID(),
		EVSEUID:           p.EVSEUID,
		IDTag:             s.tagOrDefault(p.IDTag),
		ExpiresAt:         expiresAt,
		Source:            p.Source,
		ResponseURL:       p.ResponseURL,
	})
	if err != nil {
		return core.Reservation{}, err
	}

	res, err := cp.ReserveNow(ctx, ocpp.ReserveRequest{
		ReservationID: r.ID,
		EVSEUID:       r.EVSEUID,
		IDTag:         r.IDTag,
		ExpiresAt:     r.ExpiresAt,
	})
	if err != nil {
		s.core.EndReservation(r.ID, endReason(err))
		return core.Reservation{}, err
	}
	if res.Status != ocpp.CommandAccepted {
		s.core.EndReservation(r.ID, core.ReservationRejected)
		return core.Reservation{}, refused(OpReserve, r.EVSEUID, res)
	}

	active, ok := s.core.ActivateReservation(r.ID)
	if !ok {
		// Nothing else ends a pending reservation, so this is a bug, not a
		// race worth handling.
		return core.Reservation{}, fmt.Errorf("reservation %d vanished while it was pending", r.ID)
	}
	return active, nil
}

// Cancel releases the active reservation on an EVSE.
//
// If the station rejects the cancellation, the most likely reason is that it
// no longer holds the reservation (it expired or was redeemed there first).
// Our record is then stale and would block new reservations until it timed
// out, so it ends here as well, and the refusal is reported.
func (s *Service) Cancel(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	r, ok := s.core.ActiveReservation(evseUID)
	if !ok {
		return fmt.Errorf("%s: %w", evseUID, ErrNoReservation)
	}
	cp, err := s.chargePoint()
	if err != nil {
		return err
	}

	res, err := cp.CancelReservation(ctx, r.ID)
	if err != nil {
		return err
	}
	s.core.EndReservation(r.ID, core.ReservationCancelled)
	if res.Status != ocpp.CommandAccepted {
		s.log.Warn("station refused to cancel a reservation; dropping our record of it anyway",
			"reservation_id", r.ID, "evse", evseUID, "status", res.Raw)
		return refused(OpCancel, evseUID, res)
	}
	return nil
}

// Start asks the station to start a session on an EVSE for a tag; an empty
// tag means auth.default_id_tag. It returns once the station has accepted the
// request. The session itself is reported separately, when the station sends
// it, and shows up in core.
func (s *Service) Start(ctx context.Context, evseUID, idTag string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint()
	if err != nil {
		return err
	}
	remoteStartID, err := s.core.NextRemoteStartID()
	if err != nil {
		return fmt.Errorf("recording the remote start: %w", err)
	}

	res, err := cp.RemoteStart(ctx, ocpp.RemoteStartRequest{
		RemoteStartID: remoteStartID,
		EVSEUID:       evseUID,
		IDTag:         s.tagOrDefault(idTag),
	})
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpStart, evseUID, res)
	}
	return nil
}

// Stop asks the station to end the session running on an EVSE.
func (s *Service) Stop(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	txs := s.core.ActiveTransactions(evseUID)
	switch len(txs) {
	case 0:
		return fmt.Errorf("%s: %w", evseUID, ErrNoActiveTransaction)
	case 1:
	default:
		// One EVSE, one session. Two means core missed a StopTransaction,
		// and guessing which one to stop could end the wrong session.
		ids := make([]string, len(txs))
		for i, tx := range txs {
			ids[i] = tx.ID
		}
		return fmt.Errorf("%s has %d active transactions (%s); refusing to guess which to stop",
			evseUID, len(txs), strings.Join(ids, ", "))
	}
	cp, err := s.chargePoint()
	if err != nil {
		return err
	}

	res, err := cp.RemoteStop(ctx, txs[0].ID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpStop, evseUID, res)
	}
	return nil
}

// Unlock releases the cable lock on an EVSE.
func (s *Service) Unlock(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint()
	if err != nil {
		return err
	}

	res, err := cp.UnlockConnector(ctx, evseUID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandUnlocked {
		return refused(OpUnlock, evseUID, res)
	}
	return nil
}

// TriggerStatus asks the station to report an EVSE's status now. The answer
// arrives as an ordinary status change in core.
func (s *Service) TriggerStatus(ctx context.Context, evseUID string) error {
	if err := s.checkEVSE(evseUID); err != nil {
		return err
	}
	cp, err := s.chargePoint()
	if err != nil {
		return err
	}

	res, err := cp.TriggerStatus(ctx, evseUID)
	if err != nil {
		return err
	}
	if res.Status != ocpp.CommandAccepted {
		return refused(OpTriggerStatus, evseUID, res)
	}
	return nil
}

// checkEVSE refuses an EVSE that is not in config before anything else
// happens: no id is allocated and nothing is sent.
func (s *Service) checkEVSE(evseUID string) error {
	if _, ok := s.cfg.EVSEByUID(evseUID); !ok {
		return fmt.Errorf("%w %q", ocpp.ErrUnknownEVSE, evseUID)
	}
	return nil
}

func (s *Service) chargePoint() (ocpp.ChargePoint, error) {
	return s.commands.Command(s.cfg.Charger.ID)
}

func (s *Service) tagOrDefault(tag string) string {
	if tag == "" {
		return s.cfg.Auth.DefaultIDTag
	}
	return tag
}
