#!/usr/bin/env python3
"""Pin json.dumps' answers for the audit encoder, for internal/audit to be tested against.

v1 wrote every audit line with ``json.dumps(record, ensure_ascii=False)``, and no
configuration of Go's encoding/json produces those bytes -- separators, HTML
escaping, the short \\b and \\f escapes, and U+2028/U+2029 all differ, and the last
two have no switch. internal/audit/marshal.go therefore hand-writes the encoding,
and this script is what says which bytes are the right ones.

Like tools/oracle/pin_pyyaml.py and unlike tools/oracle/py_oracle.py, this is not
part of the differential harness: it runs by hand, writes two files, and those
files are checked in. The harness compares live runs over testdata/cases.jsonl,
which holds no audit records at all -- v1 wrote the trail as a side effect of
destructive actions the harness must not perform. The encoding still has to stay
pinned after the Python is deleted at M7, which is why the expectations are a
checked-in artifact rather than a live comparison.

    python3 tools/oracle/pin_audit_json.py

Writes testdata/json_dumps/corpus.json (the values, tagged so a type survives the
round trip through a JSON file) and testdata/json_dumps/pin.json (name -> the exact
string json.dumps returned).

Lone surrogates are deliberately absent from the corpus. A Python str can hold one
and json.dumps will spell it, but the corpus file itself is JSON, and Go's decoder
substitutes U+FFFD on the way in -- so the two sides would be comparing different
inputs. There is no v1 behaviour to lose: the fields that reach the encoder are
hostnames, paths and command output, and marshal.go's byte-at-a-time default
branch already documents what it does with bytes Python could not have held.
"""

import json
import math
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# Every case is one value handed to json.dumps. Names are stable: a Go test failure
# names the case, and the name is how you find the input.
CASES = {
    # -- strings, the surface with the most disagreement -------------------------
    "str empty": "",
    "str ascii": "web-01",
    "str quote": 'a "quoted" word',
    "str backslash": "C:\\path\\to",
    "str backspace": "a\bb",
    "str formfeed": "a\fb",
    "str newline": "line1\nline2",
    "str carriage return": "a\rb",
    "str tab": "a\tb",
    "str nul": "a\x00b",
    "str control 0x01": "a\x01b",
    "str control 0x1f": "a\x1fb",
    "str del 0x7f": "a\x7fb",
    "str html chars": "<script>&</script>",
    "str ampersand in url": "https://example.test/a?b=1&c=2",
    "str latin1": "caf\xe9",
    "str cjk": "\u65e5\u672c\u8a9e",
    "str emoji": "\U0001f6a2 fleet",
    "str combining": "e\u0301",
    "str line separator": "a\u2028b",
    "str paragraph separator": "a\u2029b",
    "str nbsp": "a\xa0b",
    "str bom": "\ufeffhost",
    "str every short escape": '\\"\b\f\n\r\t',
    "str path with spaces": "/var/log/my logs/app.log",
    # -- integers ----------------------------------------------------------------
    "int zero": 0,
    "int one": 1,
    "int negative": -1,
    "int large": 9007199254740993,
    "int int64 max": 9223372036854775807,
    "int int64 min": -9223372036854775808,
    # -- floats ------------------------------------------------------------------
    "float zero": 0.0,
    "float negative zero": -0.0,
    "float half": 0.5,
    "float one point five": 1.5,
    "float third": 1 / 3,
    "float repr shortest": 0.1,
    "float integral": 3.0,
    "float exp lower edge": 1e-4,
    "float below exp lower edge": 9.999e-5,
    "float exp upper edge": 1e16,
    "float below exp upper edge": 9999999999999998.0,
    "float negative exponent": -2.5e-7,
    "float nan": math.nan,
    "float inf": math.inf,
    "float negative inf": -math.inf,
    # -- the rest of the vocabulary ----------------------------------------------
    "null": None,
    "bool true": True,
    "bool false": False,
    # -- containers --------------------------------------------------------------
    "list empty": [],
    "list ints": [1, 2, 3],
    "list mixed": [1, "two", None, True, 3.5],
    "list nested": [[1, [2]], []],
    "list of strings": ["a", "b"],
    "map empty": {},
    "map one": {"mode": "cli"},
    "map two": {"path": "/var/cache/apt", "bytes": 4096},
    "map nested": {"outer": {"inner": [1, {"deep": None}]}},
    "map key needs escaping": {'a "key"\n': 1},
    "map non ascii key": {"caf\xe9": "\u65e5"},
    "map value types": {
        "s": "x",
        "i": 7,
        "f": 1.25,
        "b": False,
        "n": None,
        "l": [1],
        "m": {"k": "v"},
    },
    # -- the shapes an audit record actually carries -----------------------------
    "operator block": {
        "unix_user": "opsadmin",
        "auth_principal": None,
        "source_ip": "10.0.0.9",
    },
    "target block": {"path": "/var/log/nginx/access.log.1", "bytes": 1073741824},
    "result block": {"ok": True, "error": None, "freed_bytes": 1073741824},
    "result failure block": {"ok": False, "error": "sha256 mismatch"},
}


def tag(v):
    """Render a value so its type survives the round trip through a JSON file.

    json.dump of the corpus would turn True into a JSON true and 1 into a JSON
    number, and Go's decoder would hand both back as float64 -- which is exactly
    the type information the encoder is being tested on. Every leaf is therefore a
    [kind, spelling] pair, and Go rebuilds the value from the kind.
    """
    if v is None:
        return ["null", ""]
    # Before int: bool is a subclass of int in Python.
    if isinstance(v, bool):
        return ["bool", "true" if v else "false"]
    if isinstance(v, int):
        return ["int", str(v)]
    if isinstance(v, float):
        if math.isnan(v):
            return ["float", "nan"]
        if math.isinf(v):
            return ["float", "inf" if v > 0 else "-inf"]
        # repr, which round-trips exactly, and which is also what json.dumps
        # itself writes for a finite float.
        return ["float", repr(v)]
    if isinstance(v, str):
        return ["str", v]
    if isinstance(v, list):
        return ["list", [tag(x) for x in v]]
    if isinstance(v, dict):
        # A list of pairs, not an object: a dict is ordered in Python and the
        # encoder writes it in that order, and a JSON object gives Go no ordered
        # decode without a token-level reader.
        return ["map", [[k, tag(x)] for k, x in v.items()]]
    raise TypeError(f"corpus value of unsupported type {type(v).__name__}")


def main():
    corpus = {name: tag(v) for name, v in CASES.items()}
    # ensure_ascii=False is v1's call, and it is the whole point: the pin has to
    # record UTF-8 passed through, not escaped.
    pin = {name: json.dumps(v, ensure_ascii=False) for name, v in CASES.items()}

    dest = ROOT / "testdata" / "json_dumps"
    dest.mkdir(parents=True, exist_ok=True)
    for stem, data in (("corpus", corpus), ("pin", pin)):
        with (dest / f"{stem}.json").open("w", encoding="utf-8") as fh:
            # ensure_ascii=True for the files themselves, so the checked-in corpus
            # is reviewable ASCII and a control character in a case cannot be lost
            # to an editor that trims it.
            json.dump(data, fh, indent=2, sort_keys=True, ensure_ascii=True)
            fh.write("\n")
    print(f"{len(pin)} cases pinned to {dest}")


if __name__ == "__main__":
    main()
