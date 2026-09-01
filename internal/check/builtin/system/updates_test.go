package system

import (
	"errors"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// withNotifier stages the MOTD fragment at the configured path, and nothing else,
// so a check reading some other path gets the not-found answer.
func withNotifier(src Source, text string) Source {
	src.ReadFile = func(name string) (string, error) {
		if name != src.NotifierPath {
			return "", errors.New("no such file")
		}
		return text, nil
	}
	return src
}

func withAPT(src Source, fake *cmdrun.Fake) Source {
	src.Run = fake
	return src
}

func bareHost() Source { return staged(fstest.MapFS{}, fstest.MapFS{}) }

// The cheap answer first: the notifier fragment is a file read and gives the same
// numbers the operator saw in the MOTD at login.
func TestTheNotifierAnswersWithoutASubprocess(t *testing.T) {
	fake := cmdrun.NewFake()
	src := withAPT(withNotifier(bareHost(), fixture.Text(t, "update_notifier/with_security.txt")), fake)

	res, _ := run(t, UpdatesID, src)

	if want := "14 packages upgradable, 8 of them security (via update-notifier)"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v; the notifier answered and apt was still run", fake.Calls())
	}
	// 8 security is past the crit bound of 5.
	if res.Status != check.StatusCrit {
		t.Errorf("status = %s, want crit", res.Status)
	}
}

// Not found, not readable and not recognisable are one answer -- try apt --
// because each has the same remedy and v1 collapses them too. The empty file is
// the routine case: the notifier writes one rather than writing a zero.
func TestEveryWayTheNotifierCanSayNothingFallsThroughToAPT(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  func() Source
	}{
		{"absent", bareHost},
		{"empty", func() Source { return withNotifier(bareHost(), "") }},
		{"unrecognisable", func() Source {
			return withNotifier(bareHost(), "nothing to see here\n")
		}},
		{"no reader wired", func() Source {
			src := bareHost()
			src.ReadFile = nil
			return src
		}},
		{"no path wired", func() Source {
			src := withNotifier(bareHost(), "5 packages can be updated.")
			src.NotifierPath = ""
			return src
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := cmdrun.NewFake().Stdout(fixture.Text(t, "apt/upgradable_three.txt"), "apt", "list", "--upgradable")
			res, _ := run(t, UpdatesID, withAPT(tc.src(), fake))

			if !fake.Called("apt", "list", "--upgradable") {
				t.Fatalf("apt was not consulted; calls = %v", fake.Calls())
			}
			if want := "3 packages upgradable, 1 of them security (via apt)"; res.Summary != want {
				t.Errorf("summary = %q, want %q", res.Summary, want)
			}
		})
	}
}

// Security alone is graded, which is v1's policy: a hundred pending updates on a
// host with a window next Tuesday is not a fault, and one unpatched CVE is.
func TestOnlyTheSecurityCountIsGraded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		status check.Status
	}{
		{"nothing pending", "0 packages can be updated.", check.StatusOK},
		{"plenty pending, none of it security", "300 packages can be updated.", check.StatusOK},
		{"one security update", "1 package can be updated.\n1 update is a security update.", check.StatusWarn},
		{"at the crit bound", "9 packages can be updated.\n5 are security updates.", check.StatusCrit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, UpdatesID, withNotifier(bareHost(), tc.text))
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary %q)", res.Status, tc.status, res.Summary)
			}
			if tc.status == check.StatusOK {
				return
			}
			if len(res.Trips) != 1 || res.Trips[0].Rule != threshold.UpdatesSecure {
				t.Errorf("trips = %+v", res.Trips)
			}
			if res.Trips[0].Subject != "security updates" {
				t.Errorf("trip subject = %q", res.Trips[0].Subject)
			}
		})
	}
}

// The source is on the metrics as a label, so a reader can tell a count that came
// from a file the notifier wrote last night from one apt worked out just now.
func TestTheAnswersSourceIsOnTheMetricsAndInTheData(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  Source
		want string
	}{
		{
			"notifier",
			withNotifier(bareHost(), fixture.Text(t, "update_notifier/no_security.txt")),
			SourceNotifier,
		},
		{
			"apt",
			withAPT(bareHost(), cmdrun.NewFake().
				Stdout(fixture.Text(t, "apt/upgradable_one.txt"), "apt", "list", "--upgradable")),
			SourceAPT,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, UpdatesID, tc.src)

			for _, name := range []string{SecurityUpdatesMetric, UpgradableMetric} {
				if got := metricNamed(t, res, name).Labels["source"]; got != tc.want {
					t.Errorf("%s is labelled %q, want %q", name, got, tc.want)
				}
			}
			status, ok := res.Data.(updateStatus)
			if !ok {
				t.Fatalf("data is %T", res.Data)
			}
			if status.Source != tc.want {
				t.Errorf("data source = %q, want %q", status.Source, tc.want)
			}
			if status.Upgradable != 1 && status.Upgradable != 3 {
				t.Errorf("upgradable = %d", status.Upgradable)
			}
		})
	}
	if SecurityUpdatesMetric != threshold.UpdatesSecure {
		t.Errorf("the metric is %q and the rule is %q", SecurityUpdatesMetric, threshold.UpdatesSecure)
	}
}

// A host with neither the notifier nor apt is one this check cannot speak about
// -- an RPM distribution, an immutable image -- and that is a fact about the
// host, not a problem with it.
func TestAHostWithNeitherSourceIsUnavailable(t *testing.T) {
	src := withAPT(bareHost(), cmdrun.NewFake().Missing("apt", "list", "--upgradable"))

	res, _ := run(t, UpdatesID, src)

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s, want unavailable", res.Status)
	}
	if want := "neither update-notifier nor apt can answer on this host"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("metrics = %v for a host that gave no counts", res.Metrics)
	}
}

// apt present and failing is a fault, unlike apt absent. A lock held by
// unattended-upgrades or a broken sources.list is a thing to fix, and reporting
// it as unavailable would keep it out of the exit code without --strict.
func TestAnAPTThatFailsIsAnErrorRatherThanAnAbsence(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake *cmdrun.Fake
		want string
	}{
		{
			"exits non-zero",
			cmdrun.NewFake().Exit(100, "", "E: Could not get lock /var/lib/dpkg/lock-frontend", "apt", "list", "--upgradable"),
			"apt list --upgradable failed",
		},
		{
			"will not run at all",
			cmdrun.NewFake().Fail(errors.New("permission denied"), "apt", "list", "--upgradable"),
			"apt list --upgradable did not run",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, UpdatesID, withAPT(bareHost(), tc.fake))
			if res.Status != check.StatusError {
				t.Errorf("status = %s, want error", res.Status)
			}
			if res.Summary != tc.want {
				t.Errorf("summary = %q, want %q", res.Summary, tc.want)
			}
			if res.Error == "" {
				t.Error("the error field is empty; nothing says what apt said")
			}
		})
	}
}

// apt exits 0 and lists what it knows when a mirror is unreachable, warning on
// stderr as it goes. Worth showing without downgrading the host, for the reason
// df's grumble is: the reading is still good for every source that answered.
func TestAPTsGrumbleIsAStepAndNotAVerdict(t *testing.T) {
	fake := cmdrun.NewFake().Respond(cmdrun.Result{
		Stdout: fixture.Text(t, "apt/upgradable_one.txt"),
		Stderr: "WARNING: apt does not have a stable CLI interface.\nsecond line",
	}, "apt", "list", "--upgradable")

	res, steps := run(t, UpdatesID, withAPT(bareHost(), fake))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %v, want one -- apt's first line only", steps)
	}
	if want := "apt: WARNING: apt does not have a stable CLI interface."; steps[0].Text != want {
		t.Errorf("step = %q, want %q", steps[0].Text, want)
	}
	if steps[0].Status != check.StatusWarn {
		t.Errorf("step status = %s", steps[0].Status)
	}
}

// Whitespace on stderr is not a complaint, and "apt: " with nothing after it is
// a line in steps[] that says nothing on every host that prints a trailing newline.
func TestAnEmptyGrumbleIsNotEmitted(t *testing.T) {
	fake := cmdrun.NewFake().Respond(cmdrun.Result{
		Stdout: fixture.Text(t, "apt/upgradable_one.txt"),
		Stderr: "\n  \n",
	}, "apt", "list", "--upgradable")

	_, steps := run(t, UpdatesID, withAPT(bareHost(), fake))

	if len(steps) != 0 {
		t.Errorf("steps = %v, want none", steps)
	}
}

// No NeedsBins on apt. The notifier fragment answers without any binary at all,
// and a presence gate would report unavailable on a host that could have answered
// from a file.
func TestTheUpdatesCheckDoesNotGateOnAPTBeingInstalled(t *testing.T) {
	spec := byID(t, UpdatesID, bareHost()).Spec()

	if len(spec.NeedsBins) != 0 {
		t.Errorf("NeedsBins = %v; the notifier path needs no binary", spec.NeedsBins)
	}
	if spec.Budget != updatesBudget {
		t.Errorf("budget = %s, want v1's apt timeout of %s", spec.Budget, updatesBudget)
	}
}
