package config_test

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wolffseb/cli-cpms/internal/config"
)

// validBase parses baseYAML, so a test can change one field on a config that
// is valid otherwise and call Validate on it. Going through the struct instead
// of YAML keeps control characters and invalid UTF-8 out of the quoting rules.
func validBase(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(baseYAML))
	if err != nil {
		t.Fatalf("base fixture must be valid, got: %v", err)
	}
	return cfg
}

// problems returns the flagged paths and messages of cfg.Validate.
func problems(t *testing.T, cfg *config.Config) (paths, msgs []string) {
	t.Helper()
	err := cfg.Validate()
	if err == nil {
		return nil, nil
	}
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("Validate returned %T, want config.ValidationErrors: %v", err, err)
	}
	for _, fe := range verrs {
		paths = append(paths, fe.Path)
		msgs = append(msgs, fe.Msg)
	}
	return paths, msgs
}

// limitField is one Location field cpms sends over OCPI with a length limit.
// The limits are OCPI 2.3.0's, mod_locations.asciidoc.
type limitField struct {
	path string
	// max is the spec's n in string(n) or CiString(n).
	max int
	// ci marks a CiString, which is printable ASCII; the others are string.
	ci  bool
	set func(c *config.Config, value string)
	// trimmed marks a value cpms sends trimmed, so trailing whitespace is
	// harmless there.
	trimmed bool
	// prep makes the rest of the config unaffected by the value under test.
	prep func(c *config.Config)
}

func limitFields() []limitField {
	return []limitField{{
		path: "location.id", max: 36, ci: true,
		set: func(c *config.Config, s string) { c.Location.ID = s },
	}, {
		path: "location.name", max: 255,
		set: func(c *config.Config, s string) { c.Location.Name = s },
		// Without ocpi.business_name, location.name is also the business
		// name, whose limit of 100 would flag the same path a second time.
		prep: func(c *config.Config) { c.OCPI.BusinessName = "Example CPO GmbH" },
	}, {
		path: "location.address", max: 255,
		set: func(c *config.Config, s string) { c.Location.Address = s },
	}, {
		path: "location.city", max: 45,
		set: func(c *config.Config, s string) { c.Location.City = s },
	}, {
		path: "location.postal_code", max: 10,
		set: func(c *config.Config, s string) { c.Location.PostalCode = s },
	}, {
		path: "location.evses[0].uid", max: 36, ci: true,
		set: func(c *config.Config, s string) { c.Location.EVSEs[0].UID = s },
	}, {
		path: "location.evses[0].connectors[0].id", max: 36, ci: true,
		set: func(c *config.Config, s string) { c.Location.EVSEs[0].Connectors[0].ID = s },
	}, {
		path: "ocpi.business_name", max: 100, trimmed: true,
		set: func(c *config.Config, s string) { c.OCPI.BusinessName = s },
	}}
}

// check sets value on a fresh valid config and returns what Validate flagged.
func (f limitField) check(t *testing.T, value string) (paths, msgs []string) {
	t.Helper()
	cfg := validBase(t)
	if f.prep != nil {
		f.prep(cfg)
	}
	f.set(cfg, value)
	return problems(t, cfg)
}

func TestLocationFieldLimitsAndCharacters(t *testing.T) {
	t.Parallel()

	for _, f := range limitFields() {
		t.Run(f.path, func(t *testing.T) {
			t.Parallel()

			// Only the field under test may be flagged, once.
			wantOne := []string{f.path}

			if paths, _ := f.check(t, strings.Repeat("a", f.max)); len(paths) != 0 {
				t.Errorf("%d characters is the limit and must pass, flagged %v", f.max, paths)
			}
			paths, msgs := f.check(t, strings.Repeat("a", f.max+1))
			if !reflect.DeepEqual(paths, wantOne) {
				t.Errorf("%d characters: flagged %v, want %v", f.max+1, paths, wantOne)
			} else if want := "at most " + strconv.Itoa(f.max) + " characters"; !strings.Contains(msgs[0], want) {
				t.Errorf("message %q does not state the limit (%q)", msgs[0], want)
			}

			if f.ci {
				// CiString is ASCII: 36 umlauts are 36 characters, but no
				// umlaut is allowed.
				paths, msgs := f.check(t, strings.Repeat("ü", f.max))
				if !reflect.DeepEqual(paths, wantOne) {
					t.Errorf("umlauts in a CiString: flagged %v, want %v", paths, wantOne)
				} else if !strings.Contains(msgs[0], "printable ASCII") {
					t.Errorf("message %q does not say ASCII", msgs[0])
				}
			} else {
				// string(n) counts characters: n umlauts are 2n bytes.
				if paths, _ := f.check(t, strings.Repeat("ü", f.max)); len(paths) != 0 {
					t.Errorf("%d umlauts is the limit and must pass, flagged %v", f.max, paths)
				}
				if paths, _ := f.check(t, strings.Repeat("ü", f.max+1)); !reflect.DeepEqual(paths, wantOne) {
					t.Errorf("%d umlauts: flagged %v, want %v", f.max+1, paths, wantOne)
				}
			}

			// None of these may be sent: OCPI allows printable characters only.
			for _, bad := range []struct{ name, value string }{
				{"newline", "ab\ncd"},
				{"carriage return", "ab\rcd"},
				{"tab", "ab\tcd"},
				{"trailing newline", "abcd\n"},
				{"NUL", "ab\x00cd"},
				{"C1 control (U+0085)", "ab\u0085cd"},
				{"zero width space (U+200B)", "ab\u200bcd"},
				{"line separator (U+2028)", "ab\u2028cd"},
				{"no-break space (U+00A0)", "ab\u00a0cd"},
				{"invalid UTF-8", "ab\xffcd"},
			} {
				if f.trimmed && bad.name == "trailing newline" {
					continue // see TestBusinessNameIsCheckedAsSent
				}
				paths, msgs := f.check(t, bad.value)
				if !reflect.DeepEqual(paths, wantOne) {
					t.Errorf("%s: flagged %v, want %v", bad.name, paths, wantOne)
					continue
				}
				if !strings.Contains(msgs[0], "printable") && !strings.Contains(msgs[0], "UTF-8") {
					t.Errorf("%s: message %q does not say what is wrong", bad.name, msgs[0])
				}
			}
		})
	}
}

func TestPrintableMessageNamesTheCharacter(t *testing.T) {
	t.Parallel()

	cfg := validBase(t)
	cfg.Location.Address = "ab\ncd"
	_, msgs := problems(t, cfg)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "U+000A at character 3") {
		t.Fatalf("messages = %q, want one naming U+000A at character 3", msgs)
	}

	cfg = validBase(t)
	cfg.Location.ID = "SITE-é"
	_, msgs = problems(t, cfg)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "U+00E9 at character 6") {
		t.Fatalf("messages = %q, want one naming U+00E9 at character 6", msgs)
	}
}

func TestOptionalLocationFieldsMayBeEmpty(t *testing.T) {
	t.Parallel()

	cfg := validBase(t)
	cfg.Location.Name = ""
	cfg.Location.PostalCode = ""
	cfg.OCPI.BusinessName = ""
	if paths, _ := problems(t, cfg); len(paths) != 0 {
		t.Fatalf("empty optional fields flagged: %v", paths)
	}
}

// location.name doubles as the business name when ocpi.business_name is
// unset, and must not be reported twice for one bad character.
func TestLocationNameIsReportedOnceAsTheBusinessName(t *testing.T) {
	t.Parallel()

	cfg := validBase(t)
	cfg.Location.Name = "Example\nSite"
	paths, _ := problems(t, cfg)
	if want := []string{"location.name"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("flagged %v, want %v", paths, want)
	}
}

// ocpi.business_name is sent trimmed, so what it ends in does not matter: a
// YAML block scalar leaves a trailing newline.
func TestBusinessNameIsCheckedAsSent(t *testing.T) {
	t.Parallel()

	cfg := validBase(t)
	cfg.OCPI.BusinessName = "Example CPO GmbH\n"
	if paths, _ := problems(t, cfg); len(paths) != 0 {
		t.Fatalf("trailing newline flagged: %v", paths)
	}
	cfg.OCPI.BusinessName = "Example\nCPO"
	if paths, _ := problems(t, cfg); !reflect.DeepEqual(paths, []string{"ocpi.business_name"}) {
		t.Fatalf("embedded newline flagged %v, want ocpi.business_name", paths)
	}
}

func TestPublicBaseURLMustBePrintable(t *testing.T) {
	t.Parallel()

	cfg := validBase(t)
	// url.Parse accepts this one; OCPI would see it in every URL we hand out.
	cfg.OCPI.PublicBaseURL = "http://cpms.example.test/oc\u2028pi"
	paths, _ := problems(t, cfg)
	if want := []string{"ocpi.public_base_url"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("flagged %v, want %v", paths, want)
	}
}

// The whole path from YAML text: a newline escaped in a double-quoted scalar
// reaches validation and is named by its key.
func TestYAMLWithNewlineInAddressIsRejected(t *testing.T) {
	t.Parallel()

	in := rep("  address: Musterstrasse 1\n", "  address: \"Musterstrasse 1\\nHinterhaus\"\n", 1)(baseYAML)
	_, err := config.Parse(strings.NewReader(in))
	var verrs config.ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("err = %v, want ValidationErrors", err)
	}
	if got, want := verrs.Paths(), []string{"location.address"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flagged %v, want %v", got, want)
	}
}
