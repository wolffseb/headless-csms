package config

import (
	_ "embed"
	"strings"
	"time"
)

// tzNames is every name in IANA tzdata, one per line, spelled canonically
// (generated from the tzdata that ships with the Go toolchain; Factory and the
// host-dependent "Local" are left out).
//
// time.LoadLocation cannot do this check on its own: it reads the host's
// zoneinfo directory, and on a case-insensitive file system (macOS) it also
// loads "europe/berlin". A receiver that matches the name exactly (Java's
// ZoneId.of) then fails on a value that validated here, and the same config
// would be rejected on Linux. Matching against this list gives every host the
// same answer.
//
//go:embed tznames.txt
var tzNames string

var tzNameSet = func() map[string]struct{} {
	lines := strings.Fields(tzNames)
	m := make(map[string]struct{}, len(lines))
	for _, n := range lines {
		m[n] = struct{}{}
	}
	return m
}()

// validTimeZone reports whether name is a canonically spelled IANA zone that
// this host can also load.
func validTimeZone(name string) bool {
	if _, ok := tzNameSet[name]; !ok {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
