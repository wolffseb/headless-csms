package cli

import "testing"

func TestPlural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		noun string
		want string
	}{
		{1, "capability", "1 capability"},
		{3, "capability", "3 capabilities"},
		{0, "capability", "0 capabilities"},
		{2, "connector", "2 connectors"},
		{2, "EVSE", "2 EVSEs"},
		// A vowel before the y keeps it.
		{2, "day", "2 days"},
		{2, "key", "2 keys"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.noun); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
		}
	}
}
