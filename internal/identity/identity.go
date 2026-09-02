// Package identity answers "who is doing this?" for the report envelope and for
// every audit record audit.Writer stamps.
//
// The unix user and the source IP are read exactly as v1 read them, because the
// audit log's byte compatibility depends on it: the same host, the same session
// and the same action must produce the same operator block whichever binary is
// installed, or a fleet mid-upgrade looks like two fleets to whatever consumes
// the trail.
//
// The auth principal is new. v1 shipped an empty identity/ package and a
// documented slot that nothing ever filled, so an SSO or second-factor identity
// existed in the dataclass and never in a record. This package fills it from two
// places and invents it from nowhere: an out-of-band mapper sets an environment
// variable, or an operator writes a static mapping in identity.yml. Neither is
// present on most hosts and the field is then empty, which is the honest answer.
// Deriving a principal from the unix user -- appending a domain, title-casing a
// login -- would put a name in the audit trail that no directory would confirm.
//
// Vendor-neutral by construction, which is a standing constraint on this project
// rather than a preference: Duo, Okta, Azure AD and GitHub SSO all reduce to
// "someone else wrote a string here", and nothing in this package knows which.
package identity

import (
	"os"
	"strings"
)

// The environment this package reads.
const (
	// SudoUserEnv is set by sudo to the login that invoked it. It comes first
	// because a Tier 2 action runs under sudo and USER would then say "root",
	// which is the one answer an audit trail must never give: it names the tool's
	// privilege rather than the person who used it.
	SudoUserEnv = "SUDO_USER"
	UserEnv     = "USER"

	// SSHConnectionEnv is sshd's "<client ip> <client port> <server ip> <server
	// port>". Absent on a console or cron session, which is not an error.
	SSHConnectionEnv = "SSH_CONNECTION"

	// AuthPrincipalEnv is where an out-of-band mapper puts the second-factor or
	// SSO identity -- a PAM hook, an SSH ForceCommand wrapper, a bastion.
	AuthPrincipalEnv = "FLEETFIX_AUTH_PRINCIPAL"
)

// UnknownUser is what v1 recorded when neither SUDO_USER nor USER was set, and
// is kept because the audit golden contains it.
//
// It is a placeholder, which everything else in this port refuses to write --
// hostinfo reports an empty distro rather than v1's "Unknown Linux". The
// difference is that a report field describes the host and may say nothing,
// while an audit record's whole purpose is to name someone; a record with an
// empty unix_user reads as a record whose identity was lost rather than as one
// taken in a session that had no identity to take. A real login named "unknown"
// would collide, and that is the cost.
const UnknownUser = "unknown"

// Operator is who this process is acting as.
//
// Empty means absent for both optional fields; there is no separate "set to the
// empty string" state, because an empty auth principal and no auth principal are
// the same fact. internal/audit maps them to JSON null on the way out, which is
// what v1 wrote.
type Operator struct {
	UnixUser      string
	AuthPrincipal string
	SourceIP      string
}

// Resolve reads the live environment.
func Resolve(principals map[string]string) Operator {
	return From(os.Getenv, principals)
}

// From resolves against an arbitrary environment lookup.
//
// A getenv function rather than t.Setenv, so the table below can run in parallel
// and so a future caller resolving an identity for some session other than this
// process's own -- the agent handling a request, say -- has somewhere to put it.
func From(getenv func(string) string, principals map[string]string) Operator {
	user := UnixUser(getenv)
	return Operator{
		UnixUser:      user,
		AuthPrincipal: AuthPrincipal(getenv, principals, user),
		SourceIP:      SourceIP(getenv(SSHConnectionEnv)),
	}
}

// UnixUser reports the login this action is attributed to.
//
// Not trimmed and not validated, matching v1 byte for byte. A login with a
// newline in it is not a login any of these variables can hold, and a
// normalisation v1 did not do is a difference in the audit trail across an
// upgrade.
func UnixUser(getenv func(string) string) string {
	for _, env := range []string{SudoUserEnv, UserEnv} {
		if v := getenv(env); v != "" {
			return v
		}
	}
	return UnknownUser
}

// SourceIP takes the client address out of an SSH_CONNECTION value.
//
// The first of four whitespace-separated fields, exactly as v1 read it, and no
// further validation: sshd writes this variable and a FleetFix that second-guessed
// it would be checking the wrong thing. An operator at the console has no
// SSH_CONNECTION and gets an empty string, which is the true answer -- they were
// not connecting from anywhere.
func SourceIP(sshConnection string) string {
	fields := strings.Fields(sshConnection)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// AuthPrincipal resolves the second-factor or SSO identity for user, or "".
//
// The environment wins over the file. A mapper that set the variable did so for
// this session and knows who authenticated; identity.yml is a static table
// written once, and a host where both are present is a host where the file has
// been outrun.
//
// Trimmed, unlike the two fields above. This one has no v1 behaviour to
// reproduce, and its plausible source is a shell wrapper doing
// FLEETFIX_AUTH_PRINCIPAL=$(...), which carries the command's trailing newline
// into the value and from there into every record of the session.
func AuthPrincipal(getenv func(string) string, principals map[string]string, user string) string {
	if v := strings.TrimSpace(getenv(AuthPrincipalEnv)); v != "" {
		return v
	}
	return strings.TrimSpace(principals[user])
}
