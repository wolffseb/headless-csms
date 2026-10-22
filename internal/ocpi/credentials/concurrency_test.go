package credentials_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/ocpi"
)

// guard bounds a wait that would otherwise hang the whole test run when the
// code under test blocks. It is a failure deadline, never a way to wait for
// something to happen.
const guard = 10 * time.Second

// gatedAuth is an Authenticator that can hold one request after it has been
// decided and before its handler runs: the window in which the registration
// can change under a request.
type gatedAuth struct {
	inner ocpi.Authenticator

	mu    sync.Mutex
	armed *hold
}

type hold struct {
	passed  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *hold) Release() { h.once.Do(func() { close(h.release) }) }

// Access decides as the inner Authenticator does, then waits if armed.
func (g *gatedAuth) Access(token string) ocpi.Access {
	access := g.inner.Access(token)
	g.mu.Lock()
	hd := g.armed
	g.armed = nil
	g.mu.Unlock()
	if hd != nil {
		close(hd.passed)
		<-hd.release
	}
	return access
}

// hold arms the gate for the next request to be decided. Wait on passed to
// know it has been decided and is held; Release lets it into its handler.
func (g *gatedAuth) hold(t *testing.T) *hold {
	t.Helper()
	hd := &hold{passed: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(hd.Release)
	g.mu.Lock()
	g.armed = hd
	g.mu.Unlock()
	return hd
}

func (hd *hold) wait(t *testing.T) {
	t.Helper()
	select {
	case <-hd.passed:
	case <-time.After(guard):
		t.Fatal("the request never reached the Authenticator")
	}
}

// async runs f in the background and returns its reply.
func async(f func() reply) <-chan reply {
	ch := make(chan reply, 1)
	go func() { ch <- f() }()
	return ch
}

func await(t *testing.T, ch <-chan reply) reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(guard):
		t.Fatal("a request did not answer")
		return reply{}
	}
}

// slowVersions makes the fake eMSP's versions answer wait until release,
// after saying it was asked.
func slowVersions(t *testing.T, emsp *fakeEMSP) (asked <-chan struct{}, release func()) {
	t.Helper()
	ask, rel := make(chan struct{}), make(chan struct{})
	var askOnce, relOnce sync.Once
	emsp.mu.Lock()
	emsp.versionsAnswer = func(w http.ResponseWriter) {
		askOnce.Do(func() { close(ask) })
		<-rel
		writeEnvelope(w, []map[string]string{{"version": ocpi.Version, "url": emsp.srv.URL + "/emsp/" + ocpi.Version}})
	}
	emsp.mu.Unlock()
	release = func() { relOnce.Do(func() { close(rel) }) }
	t.Cleanup(release)
	return ask, release
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(guard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A request is authenticated when it arrives, but the registration can change
// before its handler runs. The token it carried must then be checked against
// the state as it is by then: TOKEN_A means nothing once registered, TOKEN_C
// means nothing once unregistered.
func TestATokenThatWentStaleIsRefused(t *testing.T) {
	t.Parallel()

	const (
		a = "TOKEN_A"
		c = "TOKEN_C"
	)
	tests := []struct {
		name string
		// registered is whether a partner is registered when the stale
		// request arrives; then changes registers (from nothing) or
		// unregisters it, before the request's handler runs.
		registered bool
		token      string
		method     string
		body       bool
	}{
		{"PUT with TOKEN_A after a POST", false, a, http.MethodPut, true},
		{"DELETE with TOKEN_A after a POST", false, a, http.MethodDelete, false},
		{"GET with TOKEN_A after a POST", false, a, http.MethodGet, false},
		{"POST with TOKEN_A after a POST", false, a, http.MethodPost, true},
		{"POST with TOKEN_C after a DELETE", true, c, http.MethodPost, true},
		{"PUT with TOKEN_C after a DELETE", true, c, http.MethodPut, true},
		{"DELETE with TOKEN_C after a DELETE", true, c, http.MethodDelete, false},
		{"GET with TOKEN_C after a DELETE", true, c, http.MethodGet, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, emsp := newHarness(t), newFakeEMSP(t)
			token := map[string]string{a: h.cfg.OCPI.TokenA, c: h.cfg.OCPI.TokenC}[tc.token]
			if tc.registered {
				h.register(emsp)
			}

			var body any
			if tc.body {
				// A different TOKEN_B from the one that registers or stays.
				body = theirCredentials(newTokenB, emsp.versionsURL())
			}
			held := h.gate.hold(t)
			stale := async(func() reply { return h.do(tc.method, "credentials", token, body) })
			held.wait(t)

			// While it is held, the registration changes.
			if tc.registered {
				if r := h.do(http.MethodDelete, "credentials", h.cfg.OCPI.TokenC, nil); r.status != http.StatusOK {
					t.Fatalf("DELETE: HTTP %d", r.status)
				}
			} else {
				h.register(emsp)
			}
			calls := len(emsp.got())
			before := h.onDisk()
			held.Release()

			r := await(t, stale)
			if r.status != http.StatusUnauthorized {
				t.Errorf("stale %s: HTTP %d, status_code %d; want 401", tc.method, r.status, r.env.StatusCode)
			}
			for _, tok := range []string{h.cfg.OCPI.TokenA, h.cfg.OCPI.TokenC} {
				if strings.Contains(string(r.env.Data), tok) {
					t.Errorf("the refusal carries one of our tokens: %s", r.env.Data)
				}
			}
			after := h.onDisk()
			if (before.OCPI == nil) != (after.OCPI == nil) || before.OCPI != nil && after.OCPI.TokenB != before.OCPI.TokenB {
				t.Errorf("the stale request changed the registration: %+v -> %+v", before.OCPI, after.OCPI)
			}
			if n := len(emsp.got()); n != calls {
				t.Errorf("the stale request made the cpms call the partner %d times", n-calls)
			}
		})
	}
}

// While a registration or update calls the partner back, other changes are
// refused at once instead of queueing: a queued request would run against
// whatever registration the first one leaves behind.
func TestChangesDuringARegistrationAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		registered bool
		inflight   string // the change in progress
		token      string // the token every caller uses
		other      []string
	}{
		{"during a POST", false, http.MethodPost, "TOKEN_A", []string{http.MethodPost, http.MethodPut, http.MethodDelete}},
		{"during a PUT", true, http.MethodPut, "TOKEN_C", []string{http.MethodPut, http.MethodDelete}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, emsp := newHarness(t), newFakeEMSP(t)
			token := h.cfg.OCPI.TokenA
			if tc.registered {
				h.register(emsp)
				token = h.cfg.OCPI.TokenC
			}
			before := h.onDisk()
			asked, release := slowVersions(t, emsp)

			first := async(func() reply {
				return h.do(tc.inflight, "credentials", token, theirCredentials(newTokenB, emsp.versionsURL()))
			})
			waitFor(t, asked, "the partner to be asked for its versions")

			for _, method := range tc.other {
				var body any
				if method != http.MethodDelete {
					body = theirCredentials(tokenB+"-other", emsp.versionsURL())
				}
				r := await(t, async(func() reply { return h.do(method, "credentials", token, body) }))
				if r.env.StatusCode != 2000 || !strings.Contains(r.env.StatusMessage, "in progress") {
					t.Errorf("%s during the change: HTTP %d, status_code %d (%q); want a 2000 \"in progress\"",
						method, r.status, r.env.StatusCode, r.env.StatusMessage)
				}
			}
			// Reading our credentials changes nothing, so it is not refused.
			if r := await(t, async(func() reply { return h.do(http.MethodGet, "credentials", token, nil) })); r.status != http.StatusOK {
				t.Errorf("GET during the change: HTTP %d", r.status)
			}
			if got := h.onDisk(); (got.OCPI == nil) != (before.OCPI == nil) {
				t.Errorf("registration changed before the change finished: %+v", got.OCPI)
			}

			release()
			if r := await(t, first); r.status != http.StatusOK || r.env.StatusCode != 1000 {
				t.Fatalf("the change in progress: HTTP %d, status_code %d (%s)", r.status, r.env.StatusCode, r.env.StatusMessage)
			}
			if got := h.onDisk().OCPI; got == nil || got.TokenB != newTokenB {
				t.Errorf("stored registration = %+v, want the one that was in progress (TOKEN_B %s)", got, newTokenB)
			}
		})
	}
}

// Unregister waits for a registration in progress, but only as long as its
// context allows: past that, it fails without having changed anything, so the
// caller can report what really happened.
func TestUnregisterHonoursItsContext(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		registered bool
		inflight   string
	}{
		{"behind a POST", false, http.MethodPost},
		{"behind a PUT", true, http.MethodPut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, emsp := newHarness(t), newFakeEMSP(t)
			token := h.cfg.OCPI.TokenA
			if tc.registered {
				h.register(emsp)
				token = h.cfg.OCPI.TokenC
			}
			before := h.onDisk()
			calls := len(emsp.got())
			asked, release := slowVersions(t, emsp)

			first := async(func() reply {
				return h.do(tc.inflight, "credentials", token, theirCredentials(newTokenB, emsp.versionsURL()))
			})
			waitFor(t, asked, "the partner to be asked for its versions")

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := h.reg.Unregister(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Unregister = %v, want context.DeadlineExceeded", err)
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Errorf("Unregister took %s with a 100ms deadline", d)
			}
			if got := h.onDisk(); (got.OCPI == nil) != (before.OCPI == nil) {
				t.Errorf("a failed Unregister changed the registration: %+v", got.OCPI)
			}
			if n := len(emsp.got()); n != calls+1 {
				t.Errorf("the partner saw %d calls since, want only the versions request in progress", n-calls)
			}

			release()
			if r := await(t, first); r.status != http.StatusOK || r.env.StatusCode != 1000 {
				t.Fatalf("the change in progress: HTTP %d, status_code %d", r.status, r.env.StatusCode)
			}
			// Once it is done, Unregister goes through.
			if _, err := h.reg.Unregister(context.Background()); err != nil {
				t.Errorf("Unregister afterwards: %v", err)
			}
		})
	}
}

// Running out of time during the DELETE is a failure to tell the partner, not
// to unregister: the registration is already cleared, and Unregister says so
// instead of failing, so the CLI's exit code matches.
func TestUnregisterDeadlineDuringTheDeleteStillUnregisters(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	emsp.mu.Lock()
	emsp.deleteAnswer = func(http.ResponseWriter) { <-release }
	emsp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err := h.reg.Unregister(ctx)
	if err != nil {
		t.Fatalf("Unregister = %v, want the unregistration to be reported", err)
	}
	if got.NotifyError == "" {
		t.Error("NotifyError is empty although the partner never answered")
	}
	if h.onDisk().OCPI != nil {
		t.Error("state.json still holds the registration")
	}
}

// A body that trickles in must not hold up the operator: it is read before
// anything is locked.
func TestSlowBodyDoesNotDelayUnregister(t *testing.T) {
	t.Parallel()
	h, emsp := newHarness(t), newFakeEMSP(t)
	h.register(emsp)

	held := h.gate.hold(t)
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	slow := async(func() reply { return h.do(http.MethodPut, "credentials", h.cfg.OCPI.TokenC, pr) })
	held.wait(t)
	held.Release()
	// The handler is now reading a body that is only half there.
	go func() { _, _ = pw.Write([]byte(`{"token":"`)) }()

	ctx, cancel := context.WithTimeout(context.Background(), guard)
	defer cancel()
	if _, err := h.reg.Unregister(ctx); err != nil {
		t.Fatalf("Unregister behind a slow body: %v", err)
	}

	// The body completes after the unregistration; the PUT's TOKEN_C is stale.
	_, _ = pw.Write([]byte(newTokenB + `","url":"` + emsp.versionsURL() + `","roles":[{"role":"EMSP","party_id":"ABC","country_code":"NL"}]}`))
	pw.Close()
	if r := await(t, slow); r.status != http.StatusUnauthorized {
		t.Errorf("PUT finished after the unregistration: HTTP %d, want 401", r.status)
	}
	if h.onDisk().OCPI != nil {
		t.Error("the slow PUT brought the registration back")
	}
}
