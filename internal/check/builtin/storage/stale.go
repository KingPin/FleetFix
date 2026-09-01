package storage

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corestorage "github.com/KingPin/FleetFix/v2/internal/core/storage"
)

// A Candidate is one file old enough and matching enough to be worth an
// operator's attention. It is v1's StaleCandidate (modules/storage/stale.py:73).
type Candidate struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`

	// MtimeEpoch is whole seconds, where v1 carries the float st_mtime.
	//
	// Not a lost decimal: what a reader does with this is subtract it from the
	// report's own timestamp, and nanosecond precision on a file that has not been
	// touched for a month is noise. Whole seconds also keeps the field an integer
	// on the wire, so no consumer has to decide what 1.7469e+09 means.
	MtimeEpoch int64 `json:"mtime_epoch"`

	// Category is corestorage.CategoryArtifact or CategoryLog -- which of the two
	// glob lists claimed the name.
	Category string `json:"category"`
}

// pruneDirs is v1's PRUNE_DIR_NAMES (modules/storage/stale.py:49): package
// caches, VCS internals and virtualenvs.
//
// They hold high-churn machinery rather than the dumps and archives this scans
// for, and walking them is what makes a whole-home scan crawl -- tens of
// thousands of files under ~/.cache and node_modules alone. Skipping them is
// also the difference between the budget above being a backstop and being the
// thing that ends every run.
var pruneDirs = map[string]bool{
	".git":          true,
	".hg":           true,
	".svn":          true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".tox":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".ruff_cache":   true,
	"node_modules":  true,
	".cache":        true,
	".npm":          true,
	".cargo":        true,
	".rustup":       true,
	".gradle":       true,
	".m2":           true,
}

type stale struct {
	// now is the clock the age cutoff is measured against. Nil means time.Now; a
	// test sets it, because "older than thirty days" cannot be asserted against a
	// staged tree otherwise.
	now func() time.Time
}

func (stale) Spec() check.Spec {
	return check.Spec{
		ID:     StaleID,
		Title:  "Stale files",
		Domain: "storage",
		// No NeedsBins: the walk is syscalls, not a subprocess.
		//
		// Not Tier2 either, and that is not an oversight. Reading a directory needs
		// whatever permission that directory grants, which for the case this is for
		// -- an operator pointing it at a home -- is usually the permission they
		// already have. Where it is not, the refusal is counted and said.
		Budget: staleBudget,
		Params: []check.ParamSpec{
			{
				Name:        RootParam,
				Description: "Directory to scan, recursively.",
				Required:    true,
			},
			{
				Name:        OlderThanDaysParam,
				Description: "Only report files not modified in this many days.",
				Default:     strconv.Itoa(DefaultStaleAgeDays),
			},
		},
		InDefault: false,
	}
}

func (s stale) Run(ctx context.Context, in check.Input) check.Result {
	// No threshold lookup and no ungraded() guard: this check grades nothing, so
	// there is no rule whose absence could make it silently green. See the package
	// doc.
	root := in.Param(RootParam)

	days, err := strconv.Atoi(in.Param(OlderThanDaysParam))
	if err != nil {
		// Error rather than falling back to the default. An operator who typed
		// --param older_than_days=30d asked a question this check did not answer, and
		// answering a different one quietly is how a report comes to mean nothing.
		return check.Result{
			Status:  check.StatusError,
			Summary: OlderThanDaysParam + " must be a whole number of days",
			Error:   err.Error(),
		}
	}

	if bad, ok := walkable(root); !ok {
		return bad
	}

	cutoff := s.clock()().Add(-time.Duration(days) * 24 * time.Hour)
	found, unreadable, err := scan(ctx, root, cutoff)
	if err != nil {
		// A walk cut off partway through has counted some unknown fraction of the
		// tree, and a total that is wrong by an unknown amount is worse than no
		// total: an operator reading "1.2 GB" cannot tell it from the whole answer.
		return check.Result{
			Status:  check.StatusError,
			Summary: "the walk of " + root + " did not finish",
			Error:   err.Error(),
		}
	}

	// An unreadable subtree can hold the largest file under the root, so what
	// follows is a floor rather than a measurement, and the reader has to be told
	// which they are looking at. A warn step under an unchanged status, which is
	// logsqueeze's shape and disk.usage's before it: a home with one root-owned
	// directory in it is ordinary, and a permanent warn for it teaches an operator
	// to ignore the check.
	incomplete := ""
	if unreadable > 0 {
		incomplete = fmt.Sprintf("; %s could not be read", plural(unreadable, "path"))
		in.Progress.Emit(check.Event{
			Text: fmt.Sprintf("%s under %s could not be read, so this total is a floor",
				plural(unreadable, "path"), root),
			Status: check.StatusWarn,
		})
	}

	if len(found) == 0 {
		return check.Result{
			Status:  check.StatusOK,
			Data:    found,
			Metrics: staleTotals(root, found),
			Summary: fmt.Sprintf("nothing under %s matched and went untouched for %s%s",
				root, plural(days, "day"), incomplete),
		}
	}

	at := s.clock()()
	for i, cand := range found {
		if i == stepCap {
			in.Progress.Emit(check.Event{
				Text:   fmt.Sprintf("and %s", plural(len(found)-stepCap, "more file")),
				Status: check.StatusOK,
			})
			break
		}
		// StatusOK, like logsqueeze's candidate lines: these are the lines of a
		// reading this check does not grade, and a warn under an ok result reads as a
		// fault the status forgot to mention.
		in.Progress.Emit(check.Event{
			Text: fmt.Sprintf("%s is %s, %s old, %s",
				cand.Path, humanBytes(cand.SizeBytes), plural(ageDays(cand, at), "day"), cand.Category),
			Status: check.StatusOK,
		})
	}

	largest := found[0]
	return check.Result{
		Status:  check.StatusOK,
		Data:    found,
		Metrics: staleTotals(root, found),
		Summary: fmt.Sprintf("%s holding %s under %s, largest is %s at %s%s",
			plural(len(found), "stale file"), humanBytes(totalBytes(found)), root,
			largest.Path, humanBytes(largest.SizeBytes), incomplete),
	}
}

func (s stale) clock() func() time.Time {
	if s.now != nil {
		return s.now
	}
	return time.Now
}

// ageDays is how long ago the file was last written, in whole days.
//
// Whole days, and only in steps[] -- data[] carries the mtime instead. What a
// machine reads has to be a fact about the file rather than about when we
// happened to look, or two runs of an unchanged host produce different bytes;
// what a human reads wants "412 days", not an epoch.
func ageDays(c Candidate, at time.Time) int {
	return int(at.Sub(time.Unix(c.MtimeEpoch, 0)).Hours() / 24)
}

// staleTotals is the alerting contract: how much, how many, and how big the worst
// one is.
//
// Labelled by root, which disk.usage does by mount and for the same reason: these
// are several readings of one measurement, and an operator scanning two
// directories on a schedule needs two series rather than one that alternates. The
// largest is unlabelled by path, which logsqueeze explains -- labelling it would
// mint a new series every time the biggest file changed and leave the old one
// stale.
func staleTotals(root string, found []Candidate) []check.Metric {
	labels := map[string]string{"root": root}
	var largest int64
	if len(found) > 0 {
		largest = found[0].SizeBytes
	}
	return []check.Metric{
		gauge(StaleBytesMetric, float64(totalBytes(found)), "bytes", labels,
			"bytes held in stale artifacts and rotated logs"),
		gauge(StaleFilesMetric, float64(len(found)), "count", labels,
			"stale artifacts and rotated logs found"),
		gauge(StaleLargestMetric, float64(largest), "bytes", labels,
			"the largest stale file"),
	}
}

func totalBytes(found []Candidate) int64 {
	var total int64
	for _, c := range found {
		total += c.SizeBytes
	}
	return total
}

// walkable reports whether the root can be walked at all, and says why in the
// report's own terms when it cannot.
//
// A root that does not exist is an error here, where logsqueeze calls a missing
// /var/log unavailable. The difference is whose statement the path is: /var/log is
// this build's own default and its absence is a fact about the host, while this
// root is an argument the operator typed. Calling a mistyped path unavailable
// would exit zero without --strict, and a fleet-wide scan pointed at
// /home/alcie would stay green forever.
//
// Permission is unavailable, though, because running unprivileged is a fact about
// how the invocation was started rather than a fault in what was asked for.
func walkable(root string) (check.Result, bool) {
	info, err := os.Stat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check.Result{
			Status:  check.StatusError,
			Summary: "no such directory: " + root,
			Error:   err.Error(),
		}, false
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: "could not read " + root,
			Error:   err.Error(),
		}, false
	case !info.IsDir():
		return check.Result{
			Status:  check.StatusError,
			Summary: root + " is not a directory",
			Error:   fmt.Sprintf("%s is a %s", root, info.Mode().Type()),
		}, false
	}

	// Stat answers from the parent directory, so it says nothing about whether this
	// one can be listed. Opening it does -- and it is safe to open now that Stat has
	// confirmed it is a directory, where opening a path that turned out to be a fifo
	// would have blocked until somebody wrote to it.
	f, err := os.Open(root) //nolint:gosec // the root is the operator's argument; naming a directory is the whole check
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// Emphatically not ok: a check reporting nothing stale because it could not
			// look is the permanently-green failure this layer exists to prevent.
			return check.Result{
				Status:  check.StatusUnavailable,
				Summary: root + " is not readable by this user",
			}, false
		}
		return check.Result{
			Status:  check.StatusError,
			Summary: "could not open " + root,
			Error:   err.Error(),
		}, false
	}
	_ = f.Close()
	return check.Result{}, true
}

// scan walks the tree and returns the candidates largest-first, alongside how
// many entries refused to be read.
//
// Per-entry failures are counted rather than returned, which is v1's posture --
// os.walk's onerror discarded them, preferring partial results to an aborted scan
// -- with the swallowing made visible. The returned error is only for a walk that
// stopped: today that means the context expired.
func scan(ctx context.Context, root string, cutoff time.Time) (found []Candidate, unreadable int, err error) {
	classifier := corestorage.NewClassifier(corestorage.StaleArtifactGlobs(), corestorage.LegacyLogGlobs())
	found = []Candidate{}

	walkErr := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		// Checked per entry rather than left to the runner. The runner cuts the result
		// off at the deadline either way, but a walk that ignores its context keeps
		// issuing syscalls against a report nobody will read.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// A directory that would not open. Count it and carry on: one unreadable
			// subtree must not cost the rest of the walk.
			unreadable++
			return nil //nolint:nilerr // the count is the report; aborting would lose every other subtree
		}
		if d.IsDir() {
			// The root is never pruned, however it is named. v1 gets that for free by
			// filtering os.walk's dirnames, which never contains the root; here it has
			// to be said, or `--param root=/home/alice/.cache` would walk nothing and
			// report a clean directory.
			if name != root && pruneDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		category, ok := classifier.Classify(d.Name())
		if !ok {
			return nil
		}
		// Info after the name test, so an unreadable file this check would have
		// skipped anyway is not counted as a gap in the reading.
		info, err := d.Info()
		if err != nil {
			unreadable++
			return nil //nolint:nilerr // as above: a file that vanished mid-walk is not a reason to stop
		}
		// Regular files only. WalkDir does not follow symlinks -- which is v1's
		// follow_symlinks=False, and DirEntry.Info is the lstat that goes with it -- so
		// a symlink reaches here as a symlink and is not something to offer for
		// deletion: what an operator would be agreeing to remove is the link, while
		// the bytes the size promises to reclaim are the target's.
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		found = append(found, Candidate{
			Path:       name,
			SizeBytes:  info.Size(),
			MtimeEpoch: info.ModTime().Unix(),
			Category:   category,
		})
		return nil
	})
	if walkErr != nil {
		return nil, unreadable, walkErr
	}

	// Largest-first, which is v1's sort, with the path breaking ties. v1 leaned on
	// Python's stable sort and therefore on os.walk's order, which is the order the
	// kernel handed the directory back in -- so two runs over an unchanged tree
	// could order two equally-sized dumps differently and produce different bytes.
	slices.SortFunc(found, func(a, b Candidate) int {
		if c := cmp.Compare(b.SizeBytes, a.SizeBytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Path, b.Path)
	})
	return found, unreadable, nil
}
