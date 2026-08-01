// Package core is the port of src/fleetfix/modules: every scan and parse the three
// front doors share, with nothing above them linked in.
//
// The subpackages hold the work. This file exists for the rule they all obey:
// nothing under internal/core may depend on internal/tui, internal/cli,
// internal/check, or a terminal library, directly or transitively. That rule is
// what makes a parser testable without a screen, and what lets `check --json` and
// `fleetfix agent` run the same code the TUI does instead of a second copy of it.
//
// v1 had the same rule -- "no Textual imports inside modules/" -- as a paragraph in
// CLAUDE.md, which is to say as documentation. boundary_test.go is that paragraph
// with teeth.
package core
