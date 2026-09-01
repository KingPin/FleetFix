package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corestorage "github.com/KingPin/FleetFix/v2/internal/core/storage"
)

// An EnvIssue is one complaint about one line of a dotenv file.
//
// internal/core/storage.EnvIssue carries the offending line as well, because it
// is a parser and the differential oracle compares it against v1's dataclass.
// This one does not, and that is the point: the line that is "not in KEY=value
// form" is very often a password with a stray space in it, and a report is
// written to disk, shipped to a collector and pasted into a ticket. A line number
// and a reason are what an operator needs to go and look at it themselves.
type EnvIssue struct {
	LineNo  int    `json:"line_no"`
	Message string `json:"message"`
}

// An EnvReport is what the check found, with every value removed.
//
// Keys holds names only, sorted -- a map would marshal sorted anyway, but a
// []string says on its face that there is nothing on the other side of the colon.
type EnvReport struct {
	Path            string     `json:"path"`
	Exists          bool       `json:"exists"`
	Readable        bool       `json:"readable"`
	Keys            []string   `json:"keys"`
	MissingRequired []string   `json:"missing_required"`
	Issues          []EnvIssue `json:"issues"`
}

type env struct{}

func (env) Spec() check.Spec {
	return check.Spec{
		ID:     EnvID,
		Title:  "Dotenv file",
		Domain: "storage",
		Budget: envBudget,
		Params: []check.ParamSpec{
			{
				Name:        PathParam,
				Description: "Dotenv file to parse.",
				Required:    true,
			},
			{
				Name:        RequiredKeysParam,
				Description: "Comma-separated keys that must be present.",
			},
		},
		InDefault: false,
	}
}

func (e env) Run(_ context.Context, in check.Input) check.Result {
	path := in.Param(PathParam)
	required := splitKeys(in.Param(RequiredKeysParam))

	if bad, ok := readable(path); !ok {
		return bad
	}

	res := corestorage.CheckEnvFile(path, required)
	report := redact(res)

	if !res.Exists {
		// crit rather than unavailable, which is docker.runtime's shape and its
		// reasoning: an operator names a dotenv file because something on this host
		// depends on it, so an absent one is a definite bad state rather than an
		// expected absence -- and unavailable does not reach the exit code without
		// --strict, so a host whose .env vanished would go green.
		return check.Result{
			Status:  check.StatusCrit,
			Data:    report,
			Metrics: dotenvTotals(path, report),
			Summary: "there is no file at " + path + missingTail(report),
		}
	}

	if !res.Readable {
		return unreadable(path, report)
	}

	e.narrate(in, report)

	switch {
	case len(report.MissingRequired) > 0:
		return check.Result{
			Status:  check.StatusCrit,
			Data:    report,
			Metrics: dotenvTotals(path, report),
			Summary: fmt.Sprintf("%s is missing %s: %s%s", path,
				plural(len(report.MissingRequired), "required key"),
				strings.Join(report.MissingRequired, ", "), issuesTail(report)),
		}
	case len(report.Issues) > 0:
		// warn, not crit. A malformed line is a file the operator should look at; it
		// is not, by itself, evidence that anything is broken, because the consumer of
		// this file may never read the line that is wrong.
		return check.Result{
			Status:  check.StatusWarn,
			Data:    report,
			Metrics: dotenvTotals(path, report),
			Summary: fmt.Sprintf("%s parsed %s but %s",
				path, plural(len(report.Keys), "key"), issueCount(report)),
		}
	default:
		return check.Result{
			Status:  check.StatusOK,
			Data:    report,
			Metrics: dotenvTotals(path, report),
			Summary: fmt.Sprintf("%s parsed cleanly: %s", path, plural(len(report.Keys), "key")),
		}
	}
}

// narrate puts the findings in steps[], capped, so a file with two hundred bad
// lines does not become a report that is mostly one check.
func (env) narrate(in check.Input, report EnvReport) {
	for i, issue := range report.Issues {
		if i == stepCap {
			in.Progress.Emit(check.Event{
				Text:   fmt.Sprintf("and %s", plural(len(report.Issues)-stepCap, "more issue")),
				Status: check.StatusWarn,
			})
			break
		}
		in.Progress.Emit(check.Event{
			Text:   fmt.Sprintf("line %d: %s", issue.LineNo, issue.Message),
			Status: check.StatusWarn,
		})
	}
	for _, key := range report.MissingRequired {
		in.Progress.Emit(check.Event{
			Text:   "required key " + key + " is not set",
			Status: check.StatusCrit,
		})
	}
}

// readable rejects the paths that are not a file to parse, before anything opens
// them.
//
// The fifo case is why this runs first rather than letting CheckEnvFile find out:
// os.ReadFile on a fifo blocks until somebody writes to it, and the runner's
// deadline cuts off the result rather than the read, so the goroutine would
// outlive the report.
func readable(path string) (check.Result, bool) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Not handled here: CheckEnvFile reports a missing file as exists=false and
		// counts every required key against it, which is the answer the operator asked
		// for rather than an error about their argument.
		return check.Result{}, true
	case errors.Is(err, fs.ErrPermission):
		// Stat is answered by the parent directory, so a refusal here means a
		// directory on the way down would not be searched -- a fact about this user,
		// not about the file.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: path + " cannot be reached by this user",
		}, false
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: "could not stat " + path,
			Error:   err.Error(),
		}, false
	case info.IsDir():
		return check.Result{
			Status:  check.StatusError,
			Summary: path + " is a directory",
			Error:   path + " is a directory, and a dotenv file is a file",
		}, false
	case !info.Mode().IsRegular():
		return check.Result{
			Status:  check.StatusError,
			Summary: path + " is not a regular file",
			Error:   fmt.Sprintf("%s is a %s", path, info.Mode().Type()),
		}, false
	}
	return check.Result{}, true
}

// unreadable decides what a file that exists but would not read means.
//
// CheckEnvFile folds two different failures into one branch: the open was refused,
// or the bytes were not text. They deserve different statuses -- the first is a
// fact about this user and the second is a fact about the file -- and it costs one
// open on a path that has already failed to tell them apart. Safe to open by now:
// readable has confirmed this is a regular file.
func unreadable(path string, report EnvReport) check.Result {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's argument; naming a file is the whole check
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return check.Result{
				Status:  check.StatusUnavailable,
				Data:    report,
				Metrics: dotenvTotals(path, report),
				Summary: path + " is not readable by this user",
			}
		}
		return check.Result{
			Status:  check.StatusError,
			Data:    report,
			Metrics: dotenvTotals(path, report),
			Summary: "could not open " + path,
			Error:   err.Error(),
		}
	}
	_ = f.Close()

	// It opens, so the read failed on its contents. crit: whatever a consumer of
	// this file is expecting, it is not getting it.
	return check.Result{
		Status:  check.StatusCrit,
		Data:    report,
		Metrics: dotenvTotals(path, report),
		Summary: path + " could not be parsed as text",
		Error:   firstMessage(report),
	}
}

// dotenvTotals is the alerting contract. Labelled by path for staleTotals'
// reason: two dotenv files watched on a schedule are two series, not one that
// alternates.
func dotenvTotals(path string, report EnvReport) []check.Metric {
	labels := map[string]string{"path": path}
	return []check.Metric{
		gauge(DotenvKeysMetric, float64(len(report.Keys)), "count", labels,
			"keys parsed out of the dotenv file"),
		gauge(DotenvIssuesMetric, float64(len(report.Issues)), "count", labels,
			"lines of the dotenv file that could not be parsed"),
		gauge(DotenvMissingMetric, float64(len(report.MissingRequired)), "count", labels,
			"required keys the dotenv file does not set"),
	}
}

// redact converts the parser's result into the one this check reports: names
// without values, issues without the line they are about.
func redact(res corestorage.EnvCheckResult) EnvReport {
	keys := make([]string, 0, len(res.Keys))
	for name := range res.Keys {
		keys = append(keys, name)
	}
	sort.Strings(keys)

	issues := make([]EnvIssue, 0, len(res.Issues))
	for _, issue := range res.Issues {
		issues = append(issues, EnvIssue{LineNo: issue.LineNo, Message: issue.Message})
	}

	missing := res.MissingRequired
	if missing == nil {
		missing = []string{}
	}
	return EnvReport{
		Path:            res.Path,
		Exists:          res.Exists,
		Readable:        res.Readable,
		Keys:            keys,
		MissingRequired: missing,
		Issues:          issues,
	}
}

// splitKeys parses the comma-separated required_keys parameter.
//
// Order and duplicates both survive, because CheckEnvFile filters the caller's
// list rather than building a set from it: asking twice for the same missing key
// is reported twice, and that is v1's behaviour rather than something to correct
// on the way in.
func splitKeys(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if key := strings.TrimSpace(part); key != "" {
			out = append(out, key)
		}
	}
	return out
}

// missingTail names the required keys on a summary about a file that is not
// there, where every one of them is missing by definition and saying so is still
// worth the words: "there is no file at /srv/app/.env" is a different sentence
// from the same one that goes on to name the four keys that depended on it.
func missingTail(report EnvReport) string {
	if len(report.MissingRequired) == 0 {
		return ""
	}
	return fmt.Sprintf(", so %s are unset: %s",
		plural(len(report.MissingRequired), "required key"),
		strings.Join(report.MissingRequired, ", "))
}

func issuesTail(report EnvReport) string {
	if len(report.Issues) == 0 {
		return ""
	}
	return "; " + issueCount(report)
}

func issueCount(report EnvReport) string {
	return plural(len(report.Issues), "line") + " could not be parsed"
}

// firstMessage is the parser's own account of why the read failed, for the Error
// field. There is only ever one issue on that path, and it is not about a line.
func firstMessage(report EnvReport) string {
	if len(report.Issues) == 0 {
		return ""
	}
	return report.Issues[0].Message
}
