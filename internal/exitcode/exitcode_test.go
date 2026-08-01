package exitcode

import "testing"

// TestTheNagiosNumbersArePinned exists because these constants are a wire contract
// with every monitoring system that will ever run fleetfix, and nothing in the
// compiler stops someone from reordering them or inserting a fifth. A renumbering
// would silently turn every warning into a critical across a whole fleet.
func TestTheNagiosNumbersArePinned(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"OK", OK, 0},
		{"Warn", Warn, 1},
		{"Crit", Crit, 2},
		{"Unknown", Unknown, 3},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (monitoring-plugin convention)", tc.name, tc.got, tc.want)
		}
	}
}
