package identity

import (
	"maps"
	"os"
	"testing"
)

// env turns a table's environment into the lookup From takes. A missing key is
// the empty string, which is what os.Getenv reports and is the case most of
// these tests are about.
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// The one rule the audit trail cannot get wrong. A Tier 2 action runs the
// command under sudo, so USER reads "root" while SUDO_USER still holds the login
// that asked for it -- and a record naming root names the tool's privilege
// instead of the person who used it.
func TestSudoUserBeatsUser(t *testing.T) {
	got := UnixUser(env(map[string]string{"SUDO_USER": "alice", "USER": "root"}))
	if got != "alice" {
		t.Errorf("unix_user = %q, want alice: a sudo action was attributed to root", got)
	}
}

func TestUnixUser(t *testing.T) {
	for name, tc := range map[string]struct {
		vars map[string]string
		want string
	}{
		"sudo only":            {map[string]string{"SUDO_USER": "alice"}, "alice"},
		"user only":            {map[string]string{"USER": "bob"}, "bob"},
		"both":                 {map[string]string{"SUDO_USER": "alice", "USER": "root"}, "alice"},
		"neither":              {map[string]string{}, UnknownUser},
		"sudo set to empty":    {map[string]string{"SUDO_USER": "", "USER": "bob"}, "bob"},
		"both set to empty":    {map[string]string{"SUDO_USER": "", "USER": ""}, UnknownUser},
		"root at the console":  {map[string]string{"USER": "root"}, "root"},
		"a login with a digit": {map[string]string{"USER": "svc01"}, "svc01"},
		// Not trimmed and not validated: v1 recorded the variable as it found it,
		// and a normalisation v1 did not do is a difference in the trail across an
		// upgrade.
		"whitespace is kept": {map[string]string{"USER": " bob "}, " bob "},
	} {
		t.Run(name, func(t *testing.T) {
			if got := UnixUser(env(tc.vars)); got != tc.want {
				t.Errorf("UnixUser(%v) = %q, want %q", tc.vars, got, tc.want)
			}
		})
	}
}

func TestSourceIP(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"a v4 session":    {"203.0.113.7 54321 198.51.100.2 22", "203.0.113.7"},
		"a v6 session":    {"2001:db8::1 54321 2001:db8::2 22", "2001:db8::1"},
		"a local session": {"::1 41234 ::1 22", "::1"},
		// The console, cron, a systemd unit. Not an error and not unknown -- the
		// operator was not connecting from anywhere.
		"absent":     {"", ""},
		"whitespace": {"   ", ""},
		// sshd writes four fields. Anything else is a variable someone else set,
		// and taking the first field of it is still the most useful reading.
		"a bare address":        {"203.0.113.7", "203.0.113.7"},
		"more fields than four": {"203.0.113.7 1 2 3 4 5", "203.0.113.7"},
		"padded":                {"  203.0.113.7   54321 198.51.100.2 22  ", "203.0.113.7"},
		"tab separated":         {"203.0.113.7\t54321\t198.51.100.2\t22", "203.0.113.7"},
		// Not validated: sshd sets this, and a FleetFix that second-guessed it
		// would be checking the wrong thing.
		"not an address": {"bastion.internal 54321 198.51.100.2 22", "bastion.internal"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := SourceIP(tc.in); got != tc.want {
				t.Errorf("SourceIP(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The variable is set per session by something that watched the authentication
// happen; identity.yml is a static table written once. A host with both is a host
// where the table has been outrun.
func TestTheEnvironmentBeatsTheFile(t *testing.T) {
	got := AuthPrincipal(
		env(map[string]string{"FLEETFIX_AUTH_PRINCIPAL": "alice@duo.example"}),
		map[string]string{"alice": "alice@stale.example"},
		"alice",
	)
	if got != "alice@duo.example" {
		t.Errorf("auth_principal = %q, want the value the mapper set this session", got)
	}
}

func TestAuthPrincipal(t *testing.T) {
	for name, tc := range map[string]struct {
		vars       map[string]string
		principals map[string]string
		user       string
		want       string
	}{
		"from the environment": {
			map[string]string{"FLEETFIX_AUTH_PRINCIPAL": "alice@corp.example"}, nil, "alice", "alice@corp.example",
		},
		"from the file": {
			nil, map[string]string{"alice": "alice@corp.example"}, "alice", "alice@corp.example",
		},
		"from neither": {nil, nil, "alice", ""},
		"the file has no such user": {
			nil, map[string]string{"bob": "bob@corp.example"}, "alice", "",
		},
		"an empty variable falls through to the file": {
			map[string]string{"FLEETFIX_AUTH_PRINCIPAL": ""},
			map[string]string{"alice": "alice@corp.example"},
			"alice", "alice@corp.example",
		},
		// FLEETFIX_AUTH_PRINCIPAL=$(...) carries the command's trailing newline
		// into the value and from there into every record of the session.
		"a variable with a trailing newline": {
			map[string]string{"FLEETFIX_AUTH_PRINCIPAL": "alice@corp.example\n"}, nil, "alice", "alice@corp.example",
		},
		"a whitespace-only variable falls through": {
			map[string]string{"FLEETFIX_AUTH_PRINCIPAL": "  \n"},
			map[string]string{"alice": "alice@corp.example"},
			"alice", "alice@corp.example",
		},
		"a padded file entry": {
			nil, map[string]string{"alice": "  alice@corp.example\n"}, "alice", "alice@corp.example",
		},
		"a file entry that is only whitespace": {
			nil, map[string]string{"alice": "   "}, "alice", "",
		},
		// The unknown-user placeholder is a unix login as far as this is
		// concerned. A file that maps it is answering a question nobody should
		// ask, but it is not this function's place to refuse.
		"the unknown user is looked up like any other": {
			nil, map[string]string{UnknownUser: "nobody@corp.example"}, UnknownUser, "nobody@corp.example",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := AuthPrincipal(env(tc.vars), tc.principals, tc.user); got != tc.want {
				t.Errorf("AuthPrincipal() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Nothing derives a principal from the login. Appending a domain or title-casing
// a name would put an identity in the audit trail that no directory would
// confirm, which is worse than an empty field because it looks like evidence.
func TestAPrincipalIsNeverSynthesised(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"a plain login":      {"USER": "alice"},
		"a login with a dot": {"USER": "alice.smith"},
		"a login with an at": {"USER": "alice@corp.example"},
		"a sudo session":     {"SUDO_USER": "alice", "USER": "root"},
		"an ssh session":     {"USER": "alice", "SSH_CONNECTION": "203.0.113.7 1 198.51.100.2 22"},
		"no environment":     {},
	} {
		t.Run(name, func(t *testing.T) {
			if got := From(env(vars), nil).AuthPrincipal; got != "" {
				t.Errorf("auth_principal = %q, invented from %v", got, vars)
			}
		})
	}
}

// The three fields come from three different places, and all three are plain
// strings, so a crossed wire compiles and reports the source IP as the principal
// forever.
func TestFromFillsEachFieldFromItsOwnSource(t *testing.T) {
	vars := map[string]string{
		"SUDO_USER":               "alice",
		"USER":                    "root",
		"SSH_CONNECTION":          "203.0.113.7 54321 198.51.100.2 22",
		"FLEETFIX_AUTH_PRINCIPAL": "alice@corp.example",
	}
	want := Operator{
		UnixUser:      "alice",
		AuthPrincipal: "alice@corp.example",
		SourceIP:      "203.0.113.7",
	}
	if got := From(env(vars), nil); got != want {
		t.Errorf("From()\n got %+v\nwant %+v", got, want)
	}

	// Each variable removed in turn changes exactly the field it feeds.
	for variable, field := range map[string]func(Operator) string{
		"SSH_CONNECTION":          func(o Operator) string { return o.SourceIP },
		"FLEETFIX_AUTH_PRINCIPAL": func(o Operator) string { return o.AuthPrincipal },
	} {
		t.Run(variable, func(t *testing.T) {
			less := maps.Clone(vars)
			delete(less, variable)
			got := From(env(less), nil)
			if v := field(got); v != "" {
				t.Errorf("%s is unset and the field still reads %q", variable, v)
			}
			if got.UnixUser != want.UnixUser {
				t.Errorf("dropping %s took the unix user with it: %q", variable, got.UnixUser)
			}
		})
	}
}

// A console session as root on a host with no mapper: the shape most of the
// fleet is in, and the one where two of three fields are legitimately empty.
func TestAConsoleSessionIsMostlyEmpty(t *testing.T) {
	got := From(env(map[string]string{"USER": "root"}), nil)
	want := Operator{UnixUser: "root"}
	if got != want {
		t.Errorf("From()\n got %+v\nwant %+v", got, want)
	}
}

// The principals table is optional at the call site, not just empty.
func TestANilTableIsNotADereference(t *testing.T) {
	if got := From(env(map[string]string{"USER": "alice"}), nil).AuthPrincipal; got != "" {
		t.Errorf("auth_principal = %q", got)
	}
}

// Resolve reads this process's environment. There is nothing to compare it
// against, so it asserts the contract: it returns, and it never leaves the field
// that names someone empty.
func TestResolveReadsTheLiveEnvironment(t *testing.T) {
	got := Resolve(nil)
	if got.UnixUser == "" {
		t.Error("unix_user is empty; the fallback exists so that cannot happen")
	}
	if want := os.Getenv("SUDO_USER"); want != "" && got.UnixUser != want {
		t.Errorf("unix_user = %q, want SUDO_USER %q", got.UnixUser, want)
	}
	if got.SourceIP != SourceIP(os.Getenv("SSH_CONNECTION")) {
		t.Errorf("source_ip = %q, which is not what SSH_CONNECTION says", got.SourceIP)
	}
}
