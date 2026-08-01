package pytext

// Character classes for translating a Python `re` pattern into a Go one.
//
// Go's \s and \d are ASCII; Python's are Unicode, because a str pattern is
// Unicode by default. A pattern copied across unchanged therefore stops matching
// on exactly the input a differential harness is built to find -- a NEL line
// ending, a non-breaking space in a vendor's attribute name, an Arabic-Indic
// digit in a filename. The three v1 modules whose parsers are regex-based
// (smartctl, traceroute/tracepath, curl) all interpolate one of these.
//
// Both sets were measured, not read off the documentation. Every code point from
// 0 to 0x10FFFF was matched against Python's `re`:
//
//   - \s is 29 code points, and they are exactly str.isspace()'s -- which is
//     what IsSpace above already reproduces, so the class and the function are
//     pinned against each other by a test rather than by two copies of a list.
//   - \d is the 760 code points of category Nd, which is Go's \p{Nd} and
//     unicode.IsDigit. The count is a function of the Unicode version each
//     language was built against; the category is not, which is why the test
//     compares membership rather than a count.
const (
	// Space matches one character that Python's re \s matches.
	//
	// \p{Zl} and \p{Zp} are listed separately because U+2028 and U+2029 are LINE
	// and PARAGRAPH SEPARATOR, not Zs -- dropping them would leave two holes that
	// only turn up in text that has been through a word processor.
	Space = `[\t\n\v\f\r\x{1C}-\x{1F}\x{85}\p{Zs}\p{Zl}\p{Zp}]`

	// NotSpace matches one character that Python's re \S matches.
	NotSpace = `[^\t\n\v\f\r\x{1C}-\x{1F}\x{85}\p{Zs}\p{Zl}\p{Zp}]`

	// Digit matches one character that Python's re \d matches.
	//
	// Worth pairing with Int rather than strconv.ParseInt: Python's int() accepts
	// every one of these, so a pattern that matches a Devanagari digit run hands
	// on something ParseInt refuses.
	Digit = `[\p{Nd}]`

	// NotDigit matches one character that Python's re \D matches.
	NotDigit = `[^\p{Nd}]`
)
