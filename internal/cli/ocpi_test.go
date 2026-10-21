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
	"github.com/wolffseb/cli-cpms/internal/ocpptest"
)

// TestRunServesOCPIBesideOCPP is the end-to-end check of the OCPI listener:
// cpms run answers versions on server.ocpi_bind while the station is
// connected over OCPP, and frees both ports on shutdown.
func TestRunServesOCPIBesideOCPP(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19400, 19401
	path := runConfigPorts(t, ocppPort, ocpiPort)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, done := startRun(t, ctx, "run", "-c", path)

	// The operator copies this URL to the counterparty, so it is the
	// advertised one, not the bind address.
	waitForOutput(t, out, "OCPI versions endpoint "+cfg.OCPI.PublicBaseURL+"/versions")

	// OCPP is up at the same time: a station connects and boots.
	ocppAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ocppPort))
	client := ocpptest.MustDial(t, "ws://"+ocppAddr+"/ocpp/"+cfg.Charger.ID)
	client.MustCall("BootNotification", map[string]string{
		"chargePointVendor": "Alpitronic", "chargePointModel": "HYC300",
	})

	base, err := url.Parse(cfg.OCPI.PublicBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	ocpiAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort))
	req, err := http.NewRequest(http.MethodGet, "http://"+ocpiAddr+base.Path+"/versions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(cfg.OCPI.TokenA)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET versions: %v", err)
	}
	var env struct {
		StatusCode int `json:"status_code"`
		Data       []struct {
			Version string `json:"version"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	err = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decoding versions: %v", err)
	}
	if resp.StatusCode != http.StatusOK || env.StatusCode != 1000 {
		t.Fatalf("versions: HTTP %d, status_code %d", resp.StatusCode, env.StatusCode)
	}
	if len(env.Data) != 1 || env.Data[0].URL != cfg.OCPI.PublicBaseURL+"/2.3.0" {
		t.Errorf("versions data = %+v", env.Data)
	}

	client.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not shut down")
	}

	// Both ports are free again: nothing kept listening past shutdown.
	for _, addr := range []string{ocppAddr, ocpiAddr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("%s still in use after shutdown: %v", addr, err)
			continue
		}
		ln.Close()
	}
}

func TestRunFailsWhenTheOCPIPortIsTaken(t *testing.T) {
	t.Parallel()

	const ocppPort, ocpiPort = 19402, 19403
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ocpiPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, _, err = run(t, "run", "-c", runConfigPorts(t, ocppPort, ocpiPort))
	if err == nil {
		t.Fatal("run should fail when it cannot listen for OCPI")
	}

	// And it let go of the OCPP port on the way out.
	again, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ocppPort)))
	if err != nil {
		t.Fatalf("OCPP port still held after the failed start: %v", err)
	}
	again.Close()
}
