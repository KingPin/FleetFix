package logsqueeze

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
)

// A Candidate is one uncompressed log big enough to be worth squeezing. It is
// v1's Candidate (modules/log_squeeze/gzip_inplace.py:36), with the path
// rendered as the absolute string the report carries.
type Candidate struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

type candidates struct{ src Source }

func (candidates) Spec() check.Spec {
	return check.Spec{
		ID:    CandidatesID,
		Title: "Uncompressed logs",
		// No NeedsBins. The walk is syscalls, not a subprocess -- which is also why
		// this is one of the few checks that works identically in a scratch
		// container with no userland at all.
		Domain: "logsqueeze",
		// Not Tier2. /var/log is world-readable on a stock Debian or Ubuntu and the
		// files inside it mostly are not, but this check only needs the directory
		// entries and their sizes. Where a subtree does refuse, that is counted and
		// said rather than silently costing the total.
		Budget:    budget,
		InDefault: true,
	}
}

func (c candidates) Run(ctx context.Context, in check.Input) check.Result {
	// No threshold lookup and no ungraded() guard: this check grades nothing, so
	// there is no rule whose absence could make it silently green. See the package
	// doc for why there is no rule to begin with.
	root := c.src.root()
	fsys := c.src.fsys()

	if bad, ok := openable(fsys, root); !ok {
		return bad
	}

	found, unreadable, err := scan(ctx, fsys, root, c.src.minBytes())
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

	// An unreadable subtree can hold the largest log on the host, so what follows
	// is a floor rather than a measurement -- and the reader has to be told which
	// they are looking at.
	//
	// A warn step under an unchanged status, which is the shape disk.usage uses for
	// df's complaint and for its reason: /var/log/private and /var/log/audit refuse
	// an unprivileged reader on a stock Debian, so downgrading here would put a
	// permanent warn on every non-root run and teach an operator to ignore this
	// check. It goes in the summary as well as in steps[], because the summary is
	// the field a terse consumer reads and "no uncompressed logs" must not be the
	// whole of what it says when we could not look everywhere.
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
		// ok, and genuinely so when nothing refused: the walk completed and the host
		// has nothing over the floor. This is the one result in the domain that says
		// "no action here".
		return check.Result{
			Status:  check.StatusOK,
			Data:    found,
			Metrics: totals(found),
			Summary: fmt.Sprintf("no uncompressed logs over %s under %s%s",
				humanBytes(c.src.minBytes()), root, incomplete),
		}
	}

	largest := found[0]
	for i, cand := range found {
		if i == stepCap {
			in.Progress.Emit(check.Event{
				Text:   fmt.Sprintf("and %s over %s", plural(len(found)-stepCap, "more log"), humanBytes(c.src.minBytes())),
				Status: check.StatusOK,
			})
			break
		}
		// StatusOK, like the services domain's boot narration: these are the lines
		// of a reading this check does not grade, and a warn under an ok result
		// reads as a fault the status forgot to mention.
		in.Progress.Emit(check.Event{
			Text:   fmt.Sprintf("%s is %s", cand.Path, humanBytes(cand.SizeBytes)),
			Status: check.StatusOK,
		})
	}

	return check.Result{
		Status:  check.StatusOK,
		Data:    found,
		Metrics: totals(found),
		Summary: fmt.Sprintf("%s holding %s, largest is %s at %s%s",
			plural(len(found), "uncompressed log"), humanBytes(totalBytes(found)),
			largest.Path, humanBytes(largest.SizeBytes), incomplete),
	}
}

// totals is the alerting contract: how much, how many, and how big the worst one
// is.
//
// The largest is unlabelled, deliberately, for the reason services.boot's
// slowest metric is: labelling it by path would mint a new Prometheus series
// every time the biggest log changed and leave the old one stale. Which file it
// was belongs in the summary and in data[].
func totals(found []Candidate) []check.Metric {
	var largest int64
	if len(found) > 0 {
		largest = found[0].SizeBytes
	}
	return []check.Metric{
		gauge(BytesMetric, float64(totalBytes(found)), "bytes",
			"bytes held in uncompressed log files"),
		gauge(FilesMetric, float64(len(found)), "count",
			"uncompressed log files over the size floor"),
		gauge(LargestMetric, float64(largest), "bytes",
			"the largest uncompressed log file"),
	}
}

func totalBytes(found []Candidate) int64 {
	var total int64
	for _, c := range found {
		total += c.SizeBytes
	}
	return total
}

// openable reports whether the root can be walked at all, and says why in the
// report's own terms when it cannot.
//
// Asked before the walk because fs.WalkDir folds a root that is missing, denied
// or not a directory into the same per-entry error as any subdirectory, and
// those three deserve different words: a host with no /var/log has answered, a
// process without permission to read it has not.
func openable(fsys fs.FS, root string) (check.Result, bool) {
	info, err := fs.Stat(fsys, ".")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "this host has no " + root,
		}, false
	case errors.Is(err, fs.ErrPermission):
		// unavailable rather than error: running unprivileged is a fact about how
		// this invocation was started, not a fault in the host. It is emphatically
		// not ok -- a check that reports zero logs because it could not look is the
		// permanently-green failure this layer exists to prevent.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: root + " is not readable by this user",
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
	return check.Result{}, true
}

// scan walks the tree and returns the candidates largest-first, alongside how
// many entries refused to be read.
//
// Per-entry failures are counted rather than returned, which is v1's posture --
// it swallowed them, preferring partial results to an aborted scan -- with the
// swallowing made visible. The returned error is only for a walk that stopped:
// today that means the context expired.
func scan(ctx context.Context, fsys fs.FS, root string, minBytes int64) (found []Candidate, unreadable int, err error) {
	found = []Candidate{}
	walkErr := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		// Checked per entry rather than left to the runner. The runner cuts the
		// result off at the deadline either way, but a walk that ignores its
		// context keeps issuing syscalls against a report nobody will read.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// A directory that would not open. Count it and carry on: one unreadable
			// subtree must not cost the rest of the walk.
			unreadable++
			return nil //nolint:nilerr // the count is the report; aborting would lose every other subtree
		}
		if d.IsDir() || !squeezable(d.Name()) {
			return nil
		}
		// Info after the name test, so an unreadable file this check would have
		// skipped anyway is not counted as a gap in the reading.
		info, err := d.Info()
		if err != nil {
			unreadable++
			return nil //nolint:nilerr // as above: a file that vanished mid-walk is not a reason to stop
		}
		// Regular files only, which covers both halves of v1's `is_symlink() or not
		// is_file()`: DirEntry.Info does not follow links -- it is the lstat v1 asked
		// for with follow_symlinks=False -- and a symlink is not a regular file. That
		// matters beyond tidiness, because gzipping through a link would replace the
		// target and leave the link dangling.
		//
		// Mode rather than DirEntry.Type() because a filesystem that does not fill in
		// d_type reports the type as unknown there and correctly here.
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() < minBytes {
			return nil
		}
		found = append(found, Candidate{Path: path.Join(root, name), SizeBytes: info.Size()})
		return nil
	})
	if walkErr != nil {
		return nil, unreadable, walkErr
	}

	// Largest-first, which is v1's sort, with the path breaking ties. v1 leaned on
	// Python's stable sort and therefore on os.walk's order, which is the order the
	// kernel handed the directory back in -- so two runs over an unchanged host
	// could order two equally-sized logs differently and produce different bytes.
	slices.SortFunc(found, func(a, b Candidate) int {
		if c := cmp.Compare(b.SizeBytes, a.SizeBytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Path, b.Path)
	})
	return found, unreadable, nil
}

// squeezable is v1's _walk_logs name test (modules/log_squeeze/gzip_inplace.py:172),
// ported as written.
//
// Already-compressed files are skipped by suffix rather than by content, so a
// .log.gz that is not really gzip is skipped too. That is v1's behaviour and the
// right one: the name is what logrotate wrote, and re-compressing a file because
// its magic bytes disagreed with its name would be a surprise nobody asked for.
func squeezable(name string) bool {
	for _, ext := range []string{".gz", ".xz", ".zst", ".bz2"} {
		if strings.HasSuffix(name, ext) {
			return false
		}
	}
	if strings.HasSuffix(name, ".log") {
		return true
	}
	// Rotated logs, in both spellings logrotate produces: foo.log.1 and
	// foo.log.2026-05-16. The .partial exclusion is this domain's own leftover --
	// the squeeze writes <name>.gz.partial and unlinks it on failure, and a crash
	// between the two must not leave a file the next scan offers to compress.
	return strings.Contains(name, ".log.") && !strings.HasSuffix(name, ".partial")
}
