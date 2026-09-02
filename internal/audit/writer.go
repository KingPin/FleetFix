package audit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/identity"
)

// The three phases a record can carry.
//
// An action writes intent before it runs and result after, sharing one call_id;
// an event is a single line with neither. That split is the point of the format:
// a process killed mid-action leaves the intent line behind, so the trail says
// what was attempted even when nothing recorded what happened.
const (
	PhaseIntent = "intent"
	PhaseResult = "result"
	PhaseEvent  = "event"
)

// A Field is one key and its value inside a record's target or result.
//
// A slice of these rather than a map, because Python's dict iterates in
// insertion order and json.dumps writes it in that order. A Go map would emit
// the same keys in a different sequence on every run, which turns a trail that
// diffs cleanly across an upgrade into one that diffs everywhere.
type Field struct {
	Key string

	// Value is one of: nil, bool, string, int, int64, float64, Fields, []any.
	// Anything else is written as its Go %v rendering rather than dropped -- a
	// record that silently lost a field would be worse than one that spells a
	// value oddly, because only the second is visible to whoever reads it.
	Value any
}

// Fields is an ordered mapping. The zero value marshals as {}, which is what v1
// wrote for an action with no target.
type Fields []Field

// Set stores value under key with Python's dict semantics: an existing key keeps
// its position and takes the new value, a new one is appended.
//
// The position rule is not a detail. v1's installer reports a failure by calling
// set_result(ok=False, error=...) inside a with-block that then returns normally,
// so the context manager's {"ok": True, "error": None, **self._result} rewrites
// both fields in place. A Go version that appended a second "ok" would produce a
// record with two of them, and a JSON parser would be within its rights to
// return either.
func (f *Fields) Set(key string, value any) {
	for i := range *f {
		if (*f)[i].Key == key {
			(*f)[i].Value = value
			return
		}
	}
	*f = append(*f, Field{Key: key, Value: value})
}

// A Sink is a best-effort secondary destination for each record.
//
// It runs outside the writer's lock and its error is reported to the writer's
// OnError rather than returned, because the local file is authoritative: a
// collector that has gone away must not be able to slow down, block, or fail a
// local write. That is a standing constraint on this package, not a tuning
// choice -- an audit trail that stops when the network does is not an audit
// trail.
type Sink func(Record) error

// A Record is one line of the trail.
//
// Field order is v1's dict literal order, and the struct tags are the wire
// names. encoding/json is not what writes this -- see marshalRecord -- but the
// tags keep one spelling of each name for any consumer that unmarshals into it.
type Record struct {
	Timestamp     string   `json:"ts"`
	Host          string   `json:"host"`
	SessionID     string   `json:"session_id"`
	CallID        string   `json:"call_id"`
	Seq           int64    `json:"seq"`
	Phase         string   `json:"phase"`
	Operator      Operator `json:"operator"`
	InspectTarget string   `json:"inspect_target"`
	Action        string   `json:"action"`
	Target        Fields   `json:"target"`
	Result        Fields   `json:"result"`

	// HasResult separates a result of {} from no result at all. An intent line
	// and an event line both write result: null, and a result phase always
	// writes an object even when the action reported nothing beyond ok.
	HasResult bool `json:"-"`

	Version string `json:"fleetfix_version"`
}

// Operator is the identity block, in v1's field order. It is identity.Operator
// with the JSON shape attached; the empty strings that package uses for "absent"
// become null here, which is what v1 wrote.
type Operator struct {
	UnixUser      string `json:"unix_user"`
	AuthPrincipal string `json:"auth_principal"`
	SourceIP      string `json:"source_ip"`
}

// Options configures a Writer. Every field except Path has a working default, so
// a caller that only knows where the file goes gets v1's behaviour.
type Options struct {
	// Path is the audit log. Its parent must exist; config.Paths.AuditPath
	// creates it.
	Path string

	// Operator is who the records are attributed to.
	Operator identity.Operator

	// Version is stamped on every record as fleetfix_version.
	Version string

	// Host defaults to the kernel's hostname.
	Host string

	// SessionID defaults to a fresh UUID, shared by every record this process
	// writes. It is what joins a launch to the exit that followed it.
	SessionID string

	// InspectTarget is the account being inspected, when that differs from the
	// operator -- stamped on every record so "opsadmin inspected appuser's host"
	// stays unambiguous in a trail where both names appear.
	InspectTarget string

	// Sink is the optional OTLP export. Nil means local-only.
	Sink Sink

	// OnError is called for a failure the writer has nowhere to return: a Sink
	// that returned or panicked, and a local write from inside Do, whose error
	// must not displace the wrapped function's own. Nil discards, which is what
	// v1's logging.exception amounted to for a process with no handler
	// configured.
	//
	// One handler for both, because the two are the same event to whoever reads
	// it -- something in the audit path failed and no return value carried it.
	// A caller that treats them differently can tell them apart: a local write
	// arrives wrapped as "audit write", a sink as "audit sink", and errors.Is
	// still reaches the cause through either.
	OnError func(error)

	// Now and NewID exist so a test can assert on the exact bytes of a record.
	// Nil means the real clock and a real UUID.
	//
	// Neither has to be safe for concurrent use: the writer calls both under its
	// own lock. The real generators are safe anyway, so this buys nothing in
	// production -- it is here because the obvious test double is a counter in a
	// closure, and a package whose whole job is concurrent appends should not
	// make that a data race the caller has to find.
	Now   func() time.Time
	NewID func() string
}

// A Writer appends records to the local trail.
//
// Safe for concurrent use: the sequence number and the append are taken under
// one lock, so two goroutines cannot interleave a line or reuse a seq. The file
// is opened per write rather than held, matching v1 -- which is what lets an
// operator rotate the log out from under a long-running process without the
// records going to a deleted inode.
type Writer struct {
	opts Options

	mu  sync.Mutex
	seq int64
}

// New prepares a writer, creating the file if it is not there.
//
// The create is here rather than at the first write on purpose. A destructive
// action must not discover that its trail is unwritable after it has already
// run: the caller checks this error and refuses the action, which is the whole
// reason the local file is called authoritative. v1 did the same touch in its
// constructor, though it had no caller that acted on the failure.
func New(opts Options) (*Writer, error) {
	if opts.Host == "" {
		// v1 used platform.node(), which is the uname nodename and is what
		// os.Hostname reads. An error here leaves the field empty rather than
		// substituting a placeholder: a record naming no host is honest, and a
		// fleet-wide "unknown" would collide across every host that had one.
		opts.Host, _ = os.Hostname()
	}
	if opts.NewID == nil {
		opts.NewID = newUUID4
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SessionID == "" {
		opts.SessionID = opts.NewID()
	}

	f, err := os.OpenFile(opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: the path is the argument; naming the trail is the caller's job
	if err != nil {
		return nil, fmt.Errorf("audit trail %s is not writable: %w", opts.Path, err)
	}
	if cerr := f.Close(); cerr != nil {
		return nil, fmt.Errorf("audit trail %s is not writable: %w", opts.Path, cerr)
	}
	return &Writer{opts: opts}, nil
}

// Path reports where this writer appends, for a front door that wants to tell
// the operator where to look.
func (w *Writer) Path() string { return w.opts.Path }

// SessionID reports the id shared by every record this writer produces.
func (w *Writer) SessionID() string { return w.opts.SessionID }

// Event writes a single unpaired record: launch, exit, an escalation.
//
// Tier 1 reads are deliberately not events. The trail's value is that everything
// in it is something someone did to the host, and a line per scan would bury
// that under a hundred lines a cron job produced by looking.
func (w *Writer) Event(action string, target Fields) error {
	return w.write(Record{
		CallID: w.nextID(),
		Phase:  PhaseEvent,
		Action: action,
		Target: target,
	})
}

// A Call is one in-flight action. Do hands it to the wrapped function so the
// function can attach result fields the caller could not know in advance.
type Call struct {
	id     string
	result Fields
}

// ID is the call_id joining this action's intent and result lines.
func (c *Call) ID() string { return c.id }

// SetResult attaches a field to the result line.
//
// Setting "ok" or "error" overrides what Do would have written, in place. That
// is v1's behaviour and it is reachable, but returning an error from the wrapped
// function is the way to report a failure: it sets both fields consistently and
// cannot leave a record saying ok=false with no error beside it.
func (c *Call) SetResult(key string, value any) { c.result.Set(key, value) }

// Do writes the intent line, runs fn, then writes the paired result line.
//
// A closure rather than a returned handle the caller must remember to close,
// because a forgotten close is a trail with an intent and no result -- which
// reads exactly like a process that died mid-action, and would send someone
// looking for a crash that never happened. This is the shape of v1's
// contextmanager, for the same reason it was one there.
//
// A panic in fn is recorded as ok=false and then repanicked, so the trail
// documents the attempt before the process unwinds. The error text is
// "panic: <value>", where v1's except BaseException wrote "<ExcType>: <msg>";
// there is no Go equivalent of an exception's type name, and the word panic is
// what a reader of a Go binary's trail would search for.
//
// The returned error is fn's. A failure to write either audit line is reported
// through OnError rather than replacing it: the caller asked to do
// something, and "the disk filled while recording that it worked" is not an
// answer to whether it worked.
func (w *Writer) Do(action string, target Fields, fn func(*Call) error) error {
	call := &Call{id: w.nextID()}
	w.reportWrite(w.write(Record{
		CallID: call.id,
		Phase:  PhaseIntent,
		Action: action,
		Target: target,
	}))

	done := false
	defer func() {
		if done {
			return
		}
		// fn panicked. Record it, then let the panic continue unwinding.
		r := recover()
		w.finish(call, action, target, fmt.Errorf("panic: %v", r))
		panic(r)
	}()

	err := fn(call)
	done = true
	w.finish(call, action, target, err)
	return err
}

// finish emits the result half of a Do.
func (w *Writer) finish(call *Call, action string, target Fields, err error) {
	result := Fields{{Key: "ok", Value: err == nil}, {Key: "error", Value: nil}}
	if err != nil {
		result.Set("error", err.Error())
	}
	// The call's own fields last, so v1's set_result override still works.
	for _, f := range call.result {
		result.Set(f.Key, f.Value)
	}
	w.reportWrite(w.write(Record{
		CallID:    call.id,
		Phase:     PhaseResult,
		Action:    action,
		Target:    target,
		Result:    result,
		HasResult: true,
	}))
}

// write stamps the process-wide fields onto rec and appends it.
func (w *Writer) write(rec Record) error {
	w.mu.Lock()
	w.seq++
	rec.Seq = w.seq
	rec.Timestamp = isoMillis(w.opts.Now())
	rec.Host = w.opts.Host
	rec.SessionID = w.opts.SessionID
	rec.InspectTarget = w.opts.InspectTarget
	rec.Version = w.opts.Version
	rec.Operator = Operator{
		UnixUser:      w.opts.Operator.UnixUser,
		AuthPrincipal: w.opts.Operator.AuthPrincipal,
		SourceIP:      w.opts.Operator.SourceIP,
	}
	err := appendLine(w.opts.Path, marshalRecord(rec))
	w.mu.Unlock()

	// Outside the lock, and outside the file handle's lifetime. A collector that
	// has stopped answering must not be able to hold up the next local write.
	w.emit(rec)
	return err
}

// emit runs the sink, absorbing anything it does.
func (w *Writer) emit(rec Record) {
	if w.opts.Sink == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.report(fmt.Errorf("audit sink panicked: %v", r))
		}
	}()
	if err := w.opts.Sink(rec); err != nil {
		w.report(fmt.Errorf("audit sink: %w", err))
	}
}

// report hands a non-fatal failure to the caller's handler, if there is one.
func (w *Writer) report(err error) {
	if err == nil || w.opts.OnError == nil {
		return
	}
	w.opts.OnError(err)
}

// reportWrite is report for a local write that had nowhere to be returned. The
// prefix is what lets a handler that cares distinguish it from a sink failure;
// see Options.OnError.
func (w *Writer) reportWrite(err error) {
	if err == nil {
		return
	}
	w.report(fmt.Errorf("audit write: %w", err))
}

// appendLine opens, appends and closes, which is one syscall more than holding
// the handle and is what makes logrotate's copytruncate work as the operator
// expects.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: the path is the writer's, chosen by its caller
	if err != nil {
		return err
	}
	if _, werr := f.Write(line); werr != nil {
		_ = f.Close()
		return werr
	}
	return f.Close()
}

// isoMillis is v1's _utcnow_iso: UTC, millisecond precision, a literal Z.
//
// Truncation rather than rounding, because v1 built the string with
// microsecond // 1000 -- and a rounded 999.6ms would read as second .1000,
// which is not a time.
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000") + "Z"
}

// newUUID4 is a random UUID in the canonical hyphenated form.
//
// Hand-rolled rather than a dependency: sixteen random bytes and two bit
// fiddles, against a module graph that every unattended `check --json` in a
// fleet has to resolve. crypto/rand.Read cannot fail in Go 1.24 and later -- it
// panics on a failing entropy source rather than returning an error -- so there
// is no failure branch to write here.
func newUUID4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// nextID issues a call or session id under the writer's lock, so an injected
// generator does not have to be safe for concurrent use of its own.
func (w *Writer) nextID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.opts.NewID()
}
