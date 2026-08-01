package config

import (
	"fmt"
	"sort"
)

// PrincipalsKey is the one top-level key identity.yml carries.
//
// A key rather than a bare mapping of user to principal, so the file has
// somewhere to grow -- a default domain, a mapper command -- without the addition
// being indistinguishable from a user named after it.
const PrincipalsKey = "principals"

// Principals resolves the unix-user-to-auth-principal table from identity.yml.
//
//	principals:
//	  alice: alice@corp.example
//	  bob: bob@corp.example
//
// The file is new in v2 and optional. Most hosts will not have one, and the
// principal is then whatever FLEETFIX_AUTH_PRINCIPAL says or nothing at all;
// see internal/identity for why nothing at all is a real answer and not a
// degradation to paper over.
//
// Both layers apply, and the mapping merges key by key, so a fleet-wide
// /etc/fleetfix/identity.yml can name everyone and a personal file can correct
// one entry without restating the rest.
//
// Never fails. Every rejection is a warning naming what the operator wrote,
// because a silently dropped entry means an action attributed to a unix login
// when the operator believed it would carry their SSO identity.
func (p Paths) Principals() (map[string]string, Loaded) {
	loaded := p.Load(IdentityFile)
	out := map[string]string{}

	for _, key := range UnknownKeys(loaded.Values, PrincipalsKey) {
		loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
			"%s: no setting named %q, ignoring it", IdentityFile, key,
		))
	}

	raw, present := loaded.Values[PrincipalsKey]
	if !present {
		return out, loaded
	}
	table, ok := raw.(map[string]any)
	if !ok {
		loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
			"%s: %s must be a mapping of unix user to principal, ignoring it",
			IdentityFile, PrincipalsKey,
		))
		return out, loaded
	}

	// Sorted, so two runs over the same file produce byte-identical warnings.
	// The report is asserted byte-stable across consecutive runs, and warnings
	// travel in it.
	users := make([]string, 0, len(table))
	for user := range table {
		users = append(users, user)
	}
	sort.Strings(users)

	for _, user := range users {
		v := table[user]
		if v == nil {
			// `alice:` with nothing after it. PyYAML makes that None, and PyStr
			// would render it "None" -- a principal no directory has ever issued.
			loaded.Warnings = append(loaded.Warnings, fmt.Sprintf(
				"%s: %s.%s has no value, ignoring it", IdentityFile, PrincipalsKey, user,
			))
			continue
		}
		// PyStr rather than a type assertion: an unquoted employee number is an
		// int by the time PyYAML is done with it, and dropping it for not being a
		// string would be the same silent loss this function exists to warn about.
		out[user] = PyStr(v)
	}
	return out, loaded
}
