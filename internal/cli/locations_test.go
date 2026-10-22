package cli_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// TestRunServesLocationsBehindRegistration checks the wiring in cpms run:
// Locations is listed in version details as SENDER, and, with no
// registration yet, neither TOKEN_A nor TOKEN_C reaches it.
func TestRunServesLocationsBehindRegistration(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19500, 19501
	path := runConfigPorts(t, ocppPort, ocpiPort)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, done := startRun(t, ctx, "run", "-c", path)
	waitForOutput(t, out, "OCPI versions endpoint ")

	base, err := url.Parse(cfg.OCPI.PublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	root := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort)) + base.Path + "/2.3.0"
	get := func(p, token string) (int, json.RawMessage) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, root+p, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer resp.Body.Close()
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp.StatusCode, env.Data
	}

	code, data := get("", cfg.OCPI.TokenA)
	if code != http.StatusOK {
		t.Fatalf("version details: HTTP %d", code)
	}
	var details struct {
		Endpoints []struct{ Identifier, Role, URL string } `json:"endpoints"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range details.Endpoints {
		if e.Identifier == "locations" && e.Role == "SENDER" && e.URL == cfg.OCPI.PublicBaseURL+"/2.3.0/locations" {
			found = true
		}
	}
	if !found {
		t.Errorf("locations/SENDER missing from version details: %+v", details.Endpoints)
	}

	for name, token := range map[string]string{"TOKEN_A": cfg.OCPI.TokenA, "TOKEN_C": cfg.OCPI.TokenC} {
		if code, _ := get("/locations", token); code != http.StatusUnauthorized {
			t.Errorf("GET locations with %s before registration: HTTP %d, want 401", name, code)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down")
	}
}
