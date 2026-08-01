// Package exitcode defines FleetFix's process exit codes.
//
// The mapping is Nagios/monitoring-plugin convention, because that is what every
// consumer of a health check already understands -- Nagios, Icinga, Zabbix's
// external checks, sensu, and a hand-rolled cron wrapper all agree on it:
//
//	0 ok  1 warning  2 critical  3 unknown
//
// A leaf package with no imports so the CLI, the check command and the report
// envelope can all agree on the numbers without one of them importing another.
// Three packages each defining their own 0/1/2/3 constants is how a warning ends
// up exiting 2 on one path and 1 on another.
package exitcode

const (
	// OK means every check passed.
	OK = 0

	// Warn means at least one check tripped a warning threshold and none went
	// critical.
	Warn = 1

	// Crit means at least one check went critical.
	Crit = 2

	// Unknown means the state was not determined: a usage error, a config that
	// would not parse, a check that errored, or a build that registers no checks.
	//
	// This is also what a usage error exits with, which is a deliberate departure
	// from v1: Python's argparse exits 2 on a bad flag, and 2 means CRITICAL here.
	// A typo'd flag in an Ansible play paging someone about a critical host would
	// be a worse bug than the typo.
	Unknown = 3
)
