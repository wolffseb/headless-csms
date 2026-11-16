package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenMissingFileGivesEmptyStateAndCreatesNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if got, want := s.Get(), empty(); !reflect.DeepEqual(got, want) {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
	if s.Path() != path {
		t.Errorf("Path() = %q, want %q", s.Path(), path)
	}
	// A tool that has never been registered should leave no trace.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open created %s (stat: %v)", path, err)
	}
}

func TestRoundTripKeepsEveryFieldAndStoresUTC(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	s := mustOpen(t, path)

	// Local times with a monotonic reading: what callers will actually pass.
	berlin := time.FixedZone("CEST", 2*60*60)
	at := time.Now().In(berlin)
	want := full(at)

	if err := s.Update(func(st *State) error { *st = full(at); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	assertNoZeroFields(t, reflect.ValueOf(s.Get()), "State")

	got := mustOpen(t, path).Get()

	// The in-memory copy and a fresh read must agree exactly, or a restart
	// would change behaviour.
	if mem := s.Get(); !reflect.DeepEqual(got, mem) {
		t.Errorf("reopened state differs from in-memory state:\n got %+v\nwant %+v", got, mem)
	}

	times := map[string][2]time.Time{
		"ocpi.registered_at":         {got.OCPI.RegisteredAt, want.OCPI.RegisteredAt},
		"reservations[0].expires_at": {got.Reservations[0].ExpiresAt, want.Reservations[0].ExpiresAt},
		"reservations[0].created_at": {got.Reservations[0].CreatedAt, want.Reservations[0].CreatedAt},
		"transactions[0].started_at": {got.Transactions[0].StartedAt, want.Transactions[0].StartedAt},
	}
	for name, pair := range times {
		if pair[0].Location() != time.UTC {
			t.Errorf("%s is in %v, want UTC", name, pair[0].Location())
		}
		if !pair[0].Equal(pair[1]) {
			t.Errorf("%s = %v, want %v", name, pair[0], pair[1])
		}
	}

	// With the times normalised, everything else must come back unchanged.
	want.normalize()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the state:\n got %+v\nwant %+v", got, want)
	}

	if data := readFile(t, path); strings.Contains(data, "+02:00") {
		t.Errorf("file has a non-UTC timestamp:\n%s", data)
	}
}

func TestEmptyListsAreWrittenAsArrays(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	s := mustOpen(t, path)
	if err := s.Update(func(st *State) error {
		st.Reservations, st.Transactions = nil, nil
		st.NextReservationID = 1
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	data := readFile(t, path)
	for _, want := range []string{`"reservations": []`, `"transactions": []`, `"charging_profiles": []`, `"version": 2`} {
		if !strings.Contains(data, want) {
			t.Errorf("file does not contain %s:\n%s", want, data)
		}
	}
	if strings.Contains(data, "null") {
		t.Errorf("file contains null:\n%s", data)
	}
	if !strings.HasSuffix(data, "}\n") {
		t.Errorf("file should end with a newline:\n%q", data)
	}
	assertOnlyFile(t, filepath.Dir(path), "state.json")
}

func TestFailedWriteChangesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := mustOpen(t, path)
	if err := s.Update(func(st *State) error { st.NextReservationID = 7; return nil }); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	before := readFile(t, path)
	memBefore := s.Get()

	boom := errors.New("disk on fire")
	s.rename = func(string, string) error { return boom }

	err := s.Update(func(st *State) error {
		st.NextReservationID = 8
		st.Reservations = append(st.Reservations, Reservation{ID: 7})
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file", err)
	}

	if after := readFile(t, path); after != before {
		t.Errorf("file changed after a failed write:\nbefore %s\nafter  %s", before, after)
	}
	if got := s.Get(); !reflect.DeepEqual(got, memBefore) {
		t.Errorf("Get() changed after a failed write: %+v, want %+v", got, memBefore)
	}
	assertOnlyFile(t, dir, "state.json")
}

func TestFailedWriteOfANewFileLeavesNoTrace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := mustOpen(t, filepath.Join(dir, "state.json"))
	s.rename = func(string, string) error { return errors.New("no") }

	if err := s.Update(func(st *State) error { st.NextTransactionID = 1; return nil }); err == nil {
		t.Fatal("expected an error")
	}
	assertOnlyFile(t, dir)
}

func TestUpdateErrorChangesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := mustOpen(t, path)
	if err := s.Update(func(st *State) error { st.NextReservationID = 3; return nil }); err != nil {
		t.Fatalf("first Update: %v", err)
	}
	before := readFile(t, path)
	memBefore := s.Get()

	refused := errors.New("refused")
	err := s.Update(func(st *State) error {
		st.NextReservationID = 99
		st.Reservations = append(st.Reservations, Reservation{ID: 3})
		st.OCPI = &OCPIRegistration{TokenB: "half-done"}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("Update returned %v, want %v", err, refused)
	}

	if after := readFile(t, path); after != before {
		t.Errorf("file changed:\nbefore %s\nafter  %s", before, after)
	}
	if got := s.Get(); !reflect.DeepEqual(got, memBefore) {
		t.Errorf("Get() = %+v, want %+v", got, memBefore)
	}
	assertOnlyFile(t, dir, "state.json")
}

func TestFileIsPrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}

	path := filepath.Join(t.TempDir(), "state.json")
	s := mustOpen(t, path)
	if err := s.Update(func(st *State) error {
		st.OCPI = &OCPIRegistration{TokenB: "bearer-secret"}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600: the file holds a bearer token", mode)
	}
}

func TestOpenRefusesACorruptFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	const corrupt = `{"version":1,`
	writeFile(t, path, corrupt)

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected an error for a corrupt file")
	}
	for _, want := range []string{path, "not valid JSON", "Move it aside"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if got := readFile(t, path); got != corrupt {
		t.Errorf("corrupt file was modified: %q", got)
	}
}

func TestOpenNamesTheLineOfATypeError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	writeFile(t, path, "{\n  \"version\": 1,\n  \"next_reservation_id\": \"seven\"\n}\n")

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error %q should point at line 3", err)
	}
}

func TestOpenRefusesANewerVersion(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	const newer = `{"version": 3}`
	writeFile(t, path, newer)

	_, err := Open(path)
	if err == nil {
		t.Fatal("expected an error for a newer version")
	}
	for _, want := range []string{path, "newer cpms", "Move it aside"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if got := readFile(t, path); got != newer {
		t.Errorf("file was modified: %q", got)
	}
}

func TestOpenRefusesAFileWithoutAVersion(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]string{
		"no field":  `{"reservations": []}`,
		"version 0": `{"version": 0}`,
		"empty":     ``,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "state.json")
			writeFile(t, path, content)
			if _, err := Open(path); err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("Open(%q) = %v, want an error naming the file", content, err)
			}
		})
	}
}

func TestGetReturnsADeepCopy(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	s := mustOpen(t, path)
	if err := s.Update(func(st *State) error { *st = full(time.Now()); return nil }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Read from disk, not from Get: a shallow Get would share memory with the
	// very result the test mutates, and the comparison would prove nothing.
	want := mustOpen(t, path).Get()

	got := s.Get()
	got.NextReservationID = -1
	got.Reservations[0].IDTag = "mutated"
	got.Transactions[0].ID = "mutated"
	got.OCPI.TokenB = "mutated"
	got.OCPI.Endpoints[0].URL = "mutated"
	got.OCPI.Roles[0].PartyID = "mutated"

	if again := s.Get(); !reflect.DeepEqual(again, want) {
		t.Errorf("mutating a Get() result changed the store:\n got %+v\nwant %+v", again, want)
	}
}

func TestConcurrentUpdates(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := mustOpen(t, path)

	const n = 50
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(st *State) error { st.NextReservationID++; return nil }); err != nil {
				t.Errorf("Update: %v", err)
			}
			_ = s.Get()
		}()
	}
	wg.Wait()

	if got := s.Get().NextReservationID; got != n {
		t.Errorf("counter = %d, want %d", got, n)
	}
	if got := mustOpen(t, path).Get().NextReservationID; got != n {
		t.Errorf("counter on disk = %d, want %d", got, n)
	}
	assertOnlyFile(t, dir, "state.json")
}

// full is a state with every field set, so a round trip exercises them all.
func full(at time.Time) State {
	phases := 3
	validFrom, validTo := at, at.Add(time.Hour)
	return State{
		Version: Version,
		OCPI: &OCPIRegistration{
			RegisteredAt: at,
			TokenB:       "token-b",
			VersionsURL:  "https://emsp.example/ocpi/versions",
			Version:      "2.3.0",
			Endpoints:    []Endpoint{{Identifier: "commands", Role: "RECEIVER", URL: "https://emsp.example/ocpi/2.3.0/commands"}},
			Roles:        []PartyRole{{CountryCode: "DE", PartyID: "FRY", Role: "EMSP"}},
		},
		NextReservationID: 43,
		NextTransactionID: 1001,
		NextRemoteStartID: 5,
		Reservations: []Reservation{{
			ID:                42,
			OCPIReservationID: "c6a1f0e2-7b1d-4c1e-9d3a-5f0b8a2c9e11",
			ChargePointID:     "ALP-HYC-001",
			EVSEUID:           "EVSE-1",
			IDTag:             "04A1B2C3D4",
			ExpiresAt:         at.Add(15 * time.Minute),
			CreatedAt:         at,
			Source:            "ocpi",
			ResponseURL:       "https://emsp.example/ocpi/2.3.0/commands/RESERVE_NOW/1",
		}},
		Transactions: []Transaction{{
			ID:            "1000",
			ChargePointID: "ALP-HYC-001",
			EVSEUID:       "EVSE-1",
			ConnectorID:   1,
			IDTag:         "04A1B2C3D4",
			MeterStart:    12345,
			StartedAt:     at.Add(time.Minute),
			ReservationID: 42,
		}},
		NextChargingProfileID: 8,
		ChargingProfiles: []ChargingProfile{{
			ID:            7,
			ChargePointID: "ALP-HYC-001",
			EVSEUID:       "EVSE-1",
			Purpose:       "Tx",
			StackLevel:    2,
			Unit:          "W",
			Periods:       []ChargingPeriod{{StartSeconds: 60, Limit: 20000.5, Phases: &phases}},
			ValidFrom:     &validFrom,
			ValidTo:       &validTo,
			TransactionID: "1000",
			SetAt:         at.Add(2 * time.Minute),
			Source:        "cli",
		}},
	}
}

// assertNoZeroFields fails for any field left at its zero value, so a field
// added to State later cannot slip past the round-trip test unset.
func assertNoZeroFields(t *testing.T, v reflect.Value, path string) {
	t.Helper()

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			t.Errorf("%s is nil; set it in full()", path)
			return
		}
		assertNoZeroFields(t, v.Elem(), path)
	case reflect.Slice:
		if v.Len() == 0 {
			t.Errorf("%s is empty; set it in full()", path)
		}
		for i := range v.Len() {
			assertNoZeroFields(t, v.Index(i), path+"[]")
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			if v.Interface().(time.Time).IsZero() {
				t.Errorf("%s is zero; set it in full()", path)
			}
			return
		}
		for i := range v.NumField() {
			assertNoZeroFields(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is zero; set it in full()", path)
		}
	}
}

// assertOnlyFile fails if dir holds anything but the named files: a leftover
// temp file is a leak on every failed write.
func assertOnlyFile(t *testing.T, dir string, names ...string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, names) {
		t.Errorf("directory holds %v, want %v", got, names)
	}
}

func mustOpen(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A file written before charging profiles were recorded (version 1) loads
// with no profiles and a zero counter, and the next write migrates it.
func TestOpenMigratesVersion1(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	writeFile(t, path, `{
  "version": 1,
  "next_reservation_id": 4,
  "next_transaction_id": 9,
  "next_remote_start_id": 2,
  "reservations": [],
  "transactions": []
}
`)
	s := mustOpen(t, path)
	st := s.Get()
	if st.Version != Version || st.NextReservationID != 4 || st.NextTransactionID != 9 {
		t.Errorf("version 1 file read as %+v", st)
	}
	if st.NextChargingProfileID != 0 || st.ChargingProfiles == nil || len(st.ChargingProfiles) != 0 {
		t.Errorf("profiles = %#v, counter %d; want an empty list and 0", st.ChargingProfiles, st.NextChargingProfileID)
	}

	if err := s.Update(func(st *State) error { st.NextChargingProfileID = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	data := readFile(t, path)
	for _, want := range []string{`"version": 2`, `"charging_profiles": []`, `"next_charging_profile_id": 3`, `"next_reservation_id": 4`} {
		if !strings.Contains(data, want) {
			t.Errorf("migrated file lacks %s:\n%s", want, data)
		}
	}
}

// Get deep-copies the profiles: changing a period does not reach the store.
func TestGetDeepCopiesChargingProfiles(t *testing.T) {
	t.Parallel()

	s := mustOpen(t, filepath.Join(t.TempDir(), "state.json"))
	if err := s.Update(func(st *State) error { *st = full(time.Now()); return nil }); err != nil {
		t.Fatal(err)
	}
	got := s.Get().ChargingProfiles
	*got[0].Periods[0].Phases = 1
	got[0].Periods[0].Limit = 1
	*got[0].ValidTo = time.Time{}
	again := s.Get().ChargingProfiles[0]
	if again.Periods[0].Limit != 20000.5 || *again.Periods[0].Phases != 3 || again.ValidTo.IsZero() {
		t.Errorf("Get leaked the store's profile: %+v", again)
	}
}
