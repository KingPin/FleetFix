// Package docker holds the container-runtime logic: what is running, what it
// costs on disk, and how much of that is reclaimable.
package docker

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// decodeJSONLine decodes one line of `docker ... --format json` output the way
// json.loads does, reporting a refusal rather than an error because that is all
// v1's `except json.JSONDecodeError: continue` does with one.
//
// UseNumber is load-bearing rather than a nicety. Python's decoder produces an
// arbitrary-precision int for an integer literal, so a container ID field that
// arrived as a 20-digit number survives round-tripping there; decoding into a
// float64 here would round it and the differential would see the two disagree
// about a value neither parser touched.
//
// The Token call after Decode is what rejects trailing data. Decode stops at the
// end of the first complete value, so without it `{"a":1} {"b":2}` would be read
// as the first object, where json.loads raises and v1 drops the line.
//
// Three dialect differences remain, all of them Python accepting input Go will
// not, and none of them reachable from output either docker or podman produces:
// Python reads NaN/Infinity/-Infinity as float literals; Python keeps an unpaired
// surrogate escape where Go substitutes U+FFFD; and a nesting depth Python's
// recursive decoder cannot handle raises out of v1 rather than dropping the line.
func decodeJSONLine(line string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}
