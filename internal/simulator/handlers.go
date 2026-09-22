package simulator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpp"
	v16 "github.com/wolffseb/cli-cpms/internal/ocpp/v16"
)

// Version reports the OCPP version the simulator speaks.
func (s *Simulator) Version() ocpp.Version { return s.opts.Version }

// HandleCall answers a command from the CSMS.
//
// Responses go back before their side effects are reported, matching how a
// real station behaves: RemoteStartTransaction is accepted first, and the
// StartTransaction that follows is a separate message.
func (s *Simulator) HandleCall(ctx context.Context, _, action string, payload json.RawMessage) (any, *ocpp.RPCError) {
	// The slow scenario stalls before answering anything, which is how a CSMS
	// call timeout gets exercised.
	if s.opts.Scenario == ScenarioSlow {
		select {
		case <-time.After(s.opts.SlowDelay):
		case <-ctx.Done():
			return nil, ocpp.Errorf(ocpp.ErrInternalError, "cancelled")
		case <-s.done:
			return nil, ocpp.Errorf(ocpp.ErrInternalError, "closing")
		}
	}

	s.log.Debug("command received", "action", action)

	switch action {
	case v16.ActionReserveNow:
		return s.reserveNow(ctx, payload)
	case v16.ActionCancelReservation:
		return s.cancelReservation(payload)
	case v16.ActionRemoteStartTransaction:
		return s.remoteStart(ctx, payload)
	case v16.ActionRemoteStopTransaction:
		return s.remoteStop(ctx, payload)
	case v16.ActionUnlockConnector:
		return s.unlockConnector(payload)
	case v16.ActionTriggerMessage:
		return s.triggerMessage(ctx, payload)
	case v16.ActionGetConfiguration:
		return s.getConfiguration(payload)
	case v16.ActionChangeAvailability:
		return s.changeAvailability(ctx, payload)
	case v16.ActionReset:
		return v16.ResetConf{Status: v16.CmdAccepted}, nil
	default:
		return nil, ocpp.Errorf(ocpp.ErrNotImplemented, "action %q is not implemented", action)
	}
}

func decodeCommand[T any](payload json.RawMessage) (T, *ocpp.RPCError) {
	var v T
	if len(payload) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, ocpp.Errorf(ocpp.ErrFormationViolation, "payload does not match the action: %v", err)
	}
	return v, nil
}

func decodeInto(payload json.RawMessage, v any) error {
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, v)
}

func (s *Simulator) reserveNow(ctx context.Context, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.ReserveNowReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	switch s.opts.Scenario {
	case ScenarioRejectReserve:
		// What a station without the Reservation feature profile amounts to.
		s.log.Info("refusing reservation", "scenario", string(s.opts.Scenario))
		return v16.ReserveNowConf{Status: v16.ReservationRejected}, nil
	case ScenarioOccupied:
		return v16.ReserveNowConf{Status: v16.ReservationOccupied}, nil
	}

	s.mu.Lock()
	c, ok := s.connectors[req.ConnectorID]
	if !ok {
		s.mu.Unlock()
		return v16.ReserveNowConf{Status: v16.ReservationRejected}, nil
	}
	// A connector already in a session, or already held, cannot be reserved.
	if c.transactionID != 0 || c.status == v16.StatusCharging || c.status == v16.StatusPreparing {
		s.mu.Unlock()
		return v16.ReserveNowConf{Status: v16.ReservationOccupied}, nil
	}
	if c.status == v16.StatusFaulted || c.status == v16.StatusUnavailable {
		status := v16.ReservationFaulted
		if c.status == v16.StatusUnavailable {
			status = v16.ReservationUnavailable
		}
		s.mu.Unlock()
		return v16.ReserveNowConf{Status: status}, nil
	}
	c.reservationID = req.ReservationID
	c.status = v16.StatusReserved
	s.mu.Unlock()

	s.log.Info("reserved", "connector", req.ConnectorID,
		"reservation_id", req.ReservationID, "id_tag", req.IDTag, "expires", req.ExpiryDate)

	s.after(ctx, func(ctx context.Context) {
		_ = s.sendStatus(ctx, req.ConnectorID, v16.StatusReserved)
	})
	return v16.ReserveNowConf{Status: v16.ReservationAccepted}, nil
}

func (s *Simulator) cancelReservation(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.CancelReservationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	s.mu.Lock()
	var target int
	for id, c := range s.connectors {
		if c.reservationID == req.ReservationID {
			target = id
			break
		}
	}
	if target == 0 {
		s.mu.Unlock()
		return v16.CancelReservationConf{Status: v16.CmdRejected}, nil
	}
	c := s.connectors[target]
	c.reservationID = 0
	c.status = v16.StatusAvailable
	s.mu.Unlock()

	s.log.Info("reservation cancelled", "connector", target, "reservation_id", req.ReservationID)

	s.after(context.Background(), func(ctx context.Context) {
		_ = s.sendStatus(ctx, target, v16.StatusAvailable)
	})
	return v16.CancelReservationConf{Status: v16.CmdAccepted}, nil
}

func (s *Simulator) remoteStart(ctx context.Context, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.RemoteStartTransactionReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	connectorID := 1
	if req.ConnectorID != nil {
		connectorID = *req.ConnectorID
	} else if id, ok := s.firstFreeConnector(); ok {
		// The spec lets the station choose when no connector is named.
		connectorID = id
	}

	s.mu.Lock()
	c, ok := s.connectors[connectorID]
	if !ok || c.transactionID != 0 {
		s.mu.Unlock()
		return v16.RemoteStartTransactionConf{Status: v16.CmdRejected}, nil
	}
	s.mu.Unlock()

	idTag := req.IDTag
	s.after(ctx, func(ctx context.Context) {
		if err := s.startTransaction(ctx, connectorID, idTag); err != nil {
			s.log.Warn("starting transaction", "connector", connectorID, "error", err)
		}
	})
	return v16.RemoteStartTransactionConf{Status: v16.CmdAccepted}, nil
}

func (s *Simulator) remoteStop(ctx context.Context, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.RemoteStopTransactionReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	s.mu.Lock()
	var target int
	for id, c := range s.connectors {
		if c.transactionID == req.TransactionID && c.transactionID != 0 {
			target = id
			break
		}
	}
	s.mu.Unlock()

	if target == 0 {
		return v16.RemoteStopTransactionConf{Status: v16.CmdRejected}, nil
	}

	s.after(ctx, func(ctx context.Context) {
		if err := s.stopTransaction(ctx, target, "Remote"); err != nil {
			s.log.Warn("stopping transaction", "connector", target, "error", err)
		}
	})
	return v16.RemoteStopTransactionConf{Status: v16.CmdAccepted}, nil
}

func (s *Simulator) unlockConnector(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.UnlockConnectorReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	if s.opts.Scenario == ScenarioUnlockFails {
		return v16.UnlockConnectorConf{Status: v16.UnlockFailed}, nil
	}

	s.mu.Lock()
	_, ok := s.connectors[req.ConnectorID]
	s.mu.Unlock()
	if !ok {
		return v16.UnlockConnectorConf{Status: v16.UnlockFailed}, nil
	}

	s.log.Info("cable unlocked", "connector", req.ConnectorID)
	return v16.UnlockConnectorConf{Status: v16.UnlockUnlocked}, nil
}

func (s *Simulator) triggerMessage(ctx context.Context, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.TriggerMessageReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	connectorID := 0
	if req.ConnectorID != nil {
		connectorID = *req.ConnectorID
	}

	switch req.RequestedMessage {
	case v16.ActionBootNotification:
		s.after(ctx, func(ctx context.Context) { _ = s.boot(ctx) })
	case v16.ActionHeartbeat:
		s.after(ctx, func(ctx context.Context) {
			_, _ = s.conn.Call(ctx, v16.ActionHeartbeat, struct{}{})
		})
	case v16.ActionStatusNotification:
		status := s.stationStatus()
		if connectorID > 0 {
			status = s.ConnectorStatus(connectorID)
		}
		s.after(ctx, func(ctx context.Context) { _ = s.sendStatus(ctx, connectorID, status) })
	default:
		return v16.TriggerMessageConf{Status: v16.TriggerNotImplemented}, nil
	}

	return v16.TriggerMessageConf{Status: v16.TriggerAccepted}, nil
}

func (s *Simulator) getConfiguration(payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.GetConfigurationReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	// The Reservation profile is advertised unless the scenario says the
	// station refuses reservations, so that `cpms probe` reports the same
	// capability the station then actually honours.
	profiles := "Core,FirmwareManagement,RemoteTrigger,Reservation"
	if s.opts.Scenario == ScenarioRejectReserve {
		profiles = "Core,FirmwareManagement,RemoteTrigger"
	}

	all := []v16.ConfigurationKey{
		{Key: v16.KeySupportedFeatureProfiles, Readonly: true, Value: profiles},
		{Key: v16.KeyHeartbeatInterval, Readonly: false,
			Value: fmt.Sprintf("%d", int(s.heartbeatInterval().Seconds()))},
		{Key: v16.KeyNumberOfConnectors, Readonly: true,
			Value: fmt.Sprintf("%d", s.opts.Connectors)},
	}

	if len(req.Key) == 0 {
		return v16.GetConfigurationConf{ConfigurationKey: all}, nil
	}

	conf := v16.GetConfigurationConf{}
	for _, want := range req.Key {
		found := false
		for _, key := range all {
			if key.Key == want {
				conf.ConfigurationKey = append(conf.ConfigurationKey, key)
				found = true
				break
			}
		}
		if !found {
			conf.UnknownKey = append(conf.UnknownKey, want)
		}
	}
	return conf, nil
}

func (s *Simulator) changeAvailability(ctx context.Context, payload json.RawMessage) (any, *ocpp.RPCError) {
	req, rpcErr := decodeCommand[v16.ChangeAvailabilityReq](payload)
	if rpcErr != nil {
		return nil, rpcErr
	}

	status := v16.StatusAvailable
	if req.Type == v16.AvailabilityInoperative {
		status = v16.StatusUnavailable
	}

	// A connector mid-session would get "Scheduled" from a real station; we
	// only model the immediate case.
	s.after(ctx, func(ctx context.Context) {
		_ = s.SetConnectorStatus(ctx, req.ConnectorID, status)
	})
	return v16.ChangeAvailabilityConf{Status: v16.CmdAccepted}, nil
}

// firstFreeConnector picks the lowest-numbered idle connector.
func (s *Simulator) firstFreeConnector() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := 1; i <= s.opts.Connectors; i++ {
		if c, ok := s.connectors[i]; ok && c.transactionID == 0 {
			return i, true
		}
	}
	return 0, false
}

// startTransaction opens a session and reports it, as a station does once a
// car is actually drawing power.
func (s *Simulator) startTransaction(ctx context.Context, connectorID int, idTag string) error {
	if idTag == "" {
		idTag = s.opts.IDTag
	}

	s.mu.Lock()
	c, ok := s.connectors[connectorID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no connector %d", connectorID)
	}
	meterStart := c.meter
	s.mu.Unlock()

	// Preparing first: a car is connected but no energy flows yet.
	if err := s.SetConnectorStatus(ctx, connectorID, v16.StatusPreparing); err != nil {
		return err
	}

	raw, err := s.conn.Call(ctx, v16.ActionStartTransaction, v16.StartTransactionReq{
		ConnectorID: connectorID,
		IDTag:       idTag,
		MeterStart:  meterStart,
		Timestamp:   s.opts.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("StartTransaction: %w", err)
	}

	var conf v16.StartTransactionConf
	if err := decodeInto(raw, &conf); err != nil {
		return fmt.Errorf("StartTransaction response: %w", err)
	}
	if conf.IDTagInfo.Status != v16.AuthAccepted {
		s.log.Warn("csms refused the token", "id_tag", idTag, "status", conf.IDTagInfo.Status)
		return s.SetConnectorStatus(ctx, connectorID, v16.StatusAvailable)
	}

	s.mu.Lock()
	c.transactionID = conf.TransactionID
	// Starting a session consumes any reservation held on the connector.
	c.reservationID = 0
	s.mu.Unlock()

	s.log.Info("transaction started",
		"connector", connectorID, "transaction_id", conf.TransactionID, "id_tag", idTag)
	return s.SetConnectorStatus(ctx, connectorID, v16.StatusCharging)
}

// stopTransaction closes a session and reports it.
func (s *Simulator) stopTransaction(ctx context.Context, connectorID int, reason string) error {
	s.mu.Lock()
	c, ok := s.connectors[connectorID]
	if !ok || c.transactionID == 0 {
		s.mu.Unlock()
		return fmt.Errorf("no transaction on connector %d", connectorID)
	}
	txID := c.transactionID
	// Stand in for energy delivered, so the meter moves across a session.
	c.meter += 1000
	meterStop := c.meter
	c.transactionID = 0
	s.mu.Unlock()

	if _, err := s.conn.Call(ctx, v16.ActionStopTransaction, v16.StopTransactionReq{
		TransactionID: txID,
		MeterStop:     meterStop,
		Timestamp:     s.opts.Now().UTC().Format(time.RFC3339),
		Reason:        reason,
	}); err != nil {
		return fmt.Errorf("StopTransaction: %w", err)
	}

	s.log.Info("transaction stopped",
		"connector", connectorID, "transaction_id", txID, "reason", reason)

	// Finishing, then free: the cable is still in before the driver takes it.
	if err := s.SetConnectorStatus(ctx, connectorID, v16.StatusFinishing); err != nil {
		return err
	}
	return s.SetConnectorStatus(ctx, connectorID, v16.StatusAvailable)
}

// StartTransaction begins a session locally, standing in for someone holding
// a card to the reader.
func (s *Simulator) StartTransaction(ctx context.Context, connectorID int) error {
	return s.startTransaction(ctx, connectorID, s.opts.IDTag)
}

// StopTransaction ends a session locally.
func (s *Simulator) StopTransaction(ctx context.Context, connectorID int) error {
	return s.stopTransaction(ctx, connectorID, "Local")
}

// after runs a command's side effect once the response to that command has
// gone out, and tracks it so Close waits for it.
//
// It deliberately does not inherit the handler's context: that context belongs
// to the connection, but the work here must still complete if the caller of
// HandleCall returns first.
func (s *Simulator) after(_ context.Context, fn func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		select {
		case <-time.After(commandSettle):
		case <-s.done:
			return
		}
		fn(context.Background())
	}()
}
