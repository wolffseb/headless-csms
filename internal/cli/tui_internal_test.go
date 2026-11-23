package cli

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/tui"
)

// The log file is where to look after a crash, so a restart adds to it, and
// --log-level decides what it holds.
func TestLogFileAppendsAndHonoursTheLevel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), DefaultLogFile)
	if err := os.WriteFile(path, []byte("from the last run\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := logHandler(path, "warn")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(teeHandler{f.h, tui.NewLogSink().Handler()})
	logger.Info("an info line")
	logger.Warn("a warning", "evse", "E1")
	_ = f.f.Close()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.HasPrefix(text, "from the last run\n") {
		t.Errorf("the old log was not kept:\n%s", text)
	}
	if strings.Contains(text, "an info line") || !strings.Contains(text, `msg="a warning" evse=E1`) {
		t.Errorf("--log-level warn should keep the warning and not the info line:\n%s", text)
	}
}

func TestLogHandlerRejectsAnUnknownLevel(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), DefaultLogFile)
	if _, err := logHandler(path, "chatty"); err == nil || !strings.Contains(err.Error(), "chatty") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a bad level must not leave a file behind (stat: %v)", err)
	}
}
