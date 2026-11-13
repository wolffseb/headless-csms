package config

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	// The names must load without the host's zoneinfo, as in main.
	_ "time/tzdata"
)

func TestEveryListedTimeZoneLoads(t *testing.T) {
	t.Parallel()

	for name := range tzNameSet {
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The list is a snapshot of the toolchain's tzdata. When the toolchain gets a
// newer one with zones the list lacks, an operator would be refused a valid
// name; this fails first so the list is regenerated.
func TestTimeZoneListIsCurrent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no toolchain tzdata at %s", path)
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") || f.Name == "Factory" {
			continue
		}
		if _, ok := tzNameSet[f.Name]; !ok {
			t.Errorf("%s is in the toolchain's tzdata but not in tznames.txt; regenerate it", f.Name)
		}
	}
}
