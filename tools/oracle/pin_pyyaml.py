#!/usr/bin/env python3
"""Pin PyYAML's answers for the config loaders, for internal/config to be tested against.

v1 read config with yaml.safe_load, and PyYAML implements YAML *1.1* while
gopkg.in/yaml.v3 implements roughly YAML 1.2 core. That is a wide gap -- handing
this corpus to a naive yaml.Unmarshal agreed with PyYAML on 70 of 137 documents --
so internal/config walks yaml.v3's parse tree with PyYAML's own resolver and
constructors instead. This script is what says which answers are the right ones.

Unlike tools/oracle/py_oracle.py this is not part of the differential harness: it
runs by hand, writes two files, and those files are checked in. The harness
compares live runs of both implementations over testdata/cases.jsonl, which cannot
cover YAML dialect questions because the manifest only holds the eight real config
files an operator would write. The dialect surface is much larger than that, and it
has to stay pinned after the Python is deleted at M7 -- which is the whole reason
the expectations are a checked-in artifact rather than a live comparison.

    python3 tools/oracle/pin_pyyaml.py

Writes testdata/pyyaml/corpus.json (the documents) and testdata/pyyaml/pin.json
(PyYAML's result for each, type-tagged). Rerun it after adding a case, and read the
diff: a changed expectation means PyYAML's behaviour was measured differently than
before, which is a finding, not a formality.
"""

import datetime
import json
import math
import sys
from pathlib import Path
from tempfile import TemporaryDirectory

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "src"))

from fleetfix import config  # noqa: E402

# The corpus. Written as a dict of name -> document text so both the documents and
# the expectations are keyed the same way, and so a case can be added in one place.
#
# Sections, in order: the eight config files the manifest already holds, inlined so
# the pin does not depend on the harness; documents that are not mappings at all;
# the YAML 1.1 booleans; numbers; timestamps; structure, tags, anchors and merges;
# the rest of SafeConstructor's tag set; anchors that point at themselves; merge
# keys beyond the simple case; and keys that are not strings.
CASES = {
    # --- the manifest fixtures, inline so the pin is self-contained -----------
    "fixture/probes_full": (
        "ping:\n  targets: [10.0.0.1, 8.8.8.8]\n  count: 5\n  interval_s: 0.3\n"
        "dns:\n  names: [db.corp.internal, github.com]\n"
        "http:\n  urls: [https://api.corp.internal/health]\n"
        'tcp:\n  targets: ["db.corp.internal:5432", "https://api.corp.internal"]\n'
        "traceroute:\n  max_hops: 20\n"
        "ladder:\n  internet_target: 9.9.9.9\n  dns_name: db.corp.internal\n"
        "  https_url: https://api.corp.internal/health\n"
    ),
    "fixture/probes_broken_mapping": "ping: {count: banana\n",
    "fixture/probes_malformed": "ping: [unclosed\n",
    "fixture/top_level_list": "- 8.8.8.8\n- 1.1.1.1\n",
    "fixture/paths_target_user": "target_user: appuser\nstale_age_days: 30\n",
    "fixture/perf_true": "reduce_animations: true\n",
    "fixture/otel_full": (
        "endpoint: https://ingest.example.com:443\nservice_name: fleetfix-test\n"
        "headers:\n  x-otlp-token: abc\ninsecure: false\n"
    ),
    "fixture/otel_headers": "endpoint: https://ingest.example.com:443\nheaders:\n  a: '1'\n",
    # --- nothing, or nothing that is a mapping --------------------------------
    "empty": "",
    "blank": "\n\n  \n",
    "comment only": "# nothing here\n",
    "null document": "~\n",
    "null word": "null\n",
    "bare scalar": "foo\n",
    "bare number": "5\n",
    "empty mapping value": "a:\n",
    "empty flow mapping": "{}\n",
    "empty flow list": "[]\n",
    # --- YAML 1.1 versus 1.2 booleans ----------------------------------------
    "bool true": "a: true\n",
    "bool True": "a: True\n",
    "bool TRUE": "a: TRUE\n",
    "bool false": "a: false\n",
    "bool yes": "a: yes\n",
    "bool Yes": "a: Yes\n",
    "bool YES": "a: YES\n",
    "bool no": "a: no\n",
    "bool on": "a: on\n",
    "bool off": "a: off\n",
    "bool y": "a: y\n",
    "bool n": "a: n\n",
    "bool quoted": "a: 'true'\n",
    # --- numbers --------------------------------------------------------------
    "int": "a: 5\n",
    "int plus": "a: +5\n",
    "int minus": "a: -5\n",
    "float": "a: 0.3\n",
    "float exponent": "a: 1e3\n",
    "float exponent dotted": "a: 1.0e3\n",
    "float leading dot": "a: .5\n",
    # An exponent past the width of a double. float() saturates rather than
    # raising -- inf going up, a signed zero going down -- so the document loads.
    # Missing this rejected the whole file over one line, which for probes.yml
    # meant every *other* setting in it silently reverted to a default.
    "float overflow": "a: 1.0e+400\n",
    "float overflow negative": "a: -1.0e+400\n",
    "float underflow": "a: 1.0e-400\n",
    "float underflow negative": "a: -1.0e-400\n",
    "inf": "a: .inf\n",
    "inf caps": "a: .Inf\n",
    "neg inf": "a: -.inf\n",
    "nan": "a: .nan\n",
    "hex": "a: 0x1f\n",
    "octal yaml11": "a: 017\n",
    "octal yaml12": "a: 0o17\n",
    "underscored int": "a: 1_000\n",
    "sexagesimal": "a: 1:30\n",
    "sexagesimal float": "a: 1:30.5\n",
    "big int": "a: 9223372036854775807\n",
    "bigger than int64": "a: 9223372036854775808\n",
    "much bigger than int64": "a: 123456789012345678901234567890\n",
    "negative past int64": "a: -9223372036854775809\n",
    "leading zero": "a: 0777\n",
    "leading zero eight": "a: 08\n",
    "empty string quoted": "a: ''\n",
    # --- times ----------------------------------------------------------------
    "date": "a: 2024-01-02\n",
    "timestamp": "a: 2024-01-02T03:04:05Z\n",
    "timestamp spaced": "a: 2024-01-02 03:04:05\n",
    "clock": "a: 03:04:05\n",
    "timestamp impossible": "a: 2024-13-45\n",
    "timestamp fraction": "a: 2024-01-02T03:04:05.123456789Z\n",
    "timestamp offset": "a: 2024-01-02T03:04:05+05:30\n",
    # --- structure ------------------------------------------------------------
    "nested mapping": "a:\n  b:\n    c: 1\n",
    "list of mappings": "a:\n  - b: 1\n  - c: 2\n",
    "duplicate keys": "a: 1\na: 2\n",
    "duplicate nested keys": "outer:\n  a: 1\n  a: 2\n",
    "non-string key int": "1: a\n",
    "non-string key bool": "true: a\n",
    "non-string key null": "null: a\n",
    "non-string key float": "1.5: a\n",
    "non-string key list": "? [a, b]\n: c\n",
    "anchor and alias": "a: &x 1\nb: *x\n",
    "merge key": "base: &b {x: 1}\nchild:\n  <<: *b\n  y: 2\n",
    "explicit str tag": "a: !!str 5\n",
    "explicit int tag": 'a: !!int "5"\n',
    "unknown tag": "a: !custom 5\n",
    "python tag": "a: !!python/object:os.system {}\n",
    "binary tag": "a: !!binary aGk=\n",
    "set tag": "a: !!set {x, y}\n",
    "two documents": "a: 1\n---\nb: 2\n",
    "document start only": "---\na: 1\n",
    "document end": "a: 1\n...\n",
    "tab indent": "a:\n\tb: 1\n",
    "unclosed quote": "a: 'x\n",
    "bad indent": "a: 1\n  b: 2\n",
    "multiline scalar": "a: |\n  one\n  two\n",
    "folded scalar": "a: >\n  one\n  two\n",
    "utf8 value": "a: caf\u00e9\n",
    "nbsp value": "a: x\u00a0y\n",
    "bom": "\ufeffa: 1\n",
    "crlf": "a: 1\r\nb: 2\r\n",
    "cr only": "a: 1\rb: 2\r",
    "nel line break": "a: 1\u0085b: 2\n",
    "colon in value": "a: b:c\n",
    "hash not a comment": "a: b#c\n",
    "trailing comment": "a: 1 # note\n",
    "key with space": "a b: 1\n",
    "empty key": ": 1\n",
    "very deep": (
        "a:\n"
        + "".join(f"{' ' * (i * 2 + 2)}k{i}:\n" for i in range(1, 20))
        + "    " * 2
        + "z: 1\n"
    ),
    # --- the rest of SafeConstructor's tag set, and the tags' laxer parsing ----
    "omap tag": "a: !!omap\n  - x: 1\n  - y: 2\n",
    "pairs tag": "a: !!pairs\n  - x: 1\n  - x: 2\n",
    "omap not a sequence": "a: !!omap {x: 1}\n",
    "set tag with values": "a: !!set\n  x: 1\n",
    # Go's set keys are strings, Python's are the constructed values. With string
    # members the two are indistinguishable, so this case makes the difference show.
    "set tag int members": "a: !!set\n  1:\n  2:\n",
    "top level set": "!!set {x, y}\n",
    "explicit bool tag": "a: !!bool yes\n",
    "explicit bool tag y": "a: !!bool y\n",
    "explicit int tag octal": 'a: !!int "0o17"\n',
    "explicit int tag sexagesimal": 'a: !!int "1:30"\n',
    "explicit float tag exponent": 'a: !!float "1e3"\n',
    "explicit float tag word": 'a: !!float "banana"\n',
    "explicit null tag": "a: !!null banana\n",
    "explicit timestamp tag": "a: !!timestamp 2024-01-02\n",
    "explicit map tag": "a: !!map {x: 1}\n",
    "explicit seq tag": "a: !!seq [1, 2]\n",
    "binary tag invalid": "a: !!binary '!!!'\n",
    "binary tag multiline": "a: !!binary |\n  aGk=\n",
    # --- anchors that point at themselves, and at each other repeatedly -------
    "recursive anchor": "a: &x [*x]\n",
    "recursive anchor mapping": "a: &x {self: *x}\n",
    "alias reused": "a: &x {k: 1}\nb: *x\nc: *x\n",
    "alias to scalar reused": "a: &x 1\nb: [*x, *x, *x]\n",
    "undefined alias": "a: *nope\n",
    "expanding aliases": "a: &a [x, x]\nb: &b [*a, *a]\nc: &c [*b, *b]\nd: [*c, *c]\n",
    # --- merge keys beyond the simple case ------------------------------------
    "merge list of mappings": "l: &l {x: 1}\nr: &r {y: 2}\nc:\n  <<: [*l, *r]\n  z: 3\n",
    "merge overridden": "b: &b {x: 1}\nc:\n  <<: *b\n  x: 2\n",
    "merge of a scalar": "c:\n  <<: 1\n",
    "merge as a value": "a: <<\n",
    "recursive merge": "a: &x {<<: *x, y: 1}\n",
    "explicit key form": "? a\n: 1\n",
    "flow mapping value": "a: {}\n",
    "flow list value": "a: []\n",
    # --- keys that are not strings, rendered as json.dumps would key them -----
    "tilde key": "~: a\n",
    "value key": "=: 1\n",
    "quoted numeric key": "'1': a\n",
    "colliding keys": "1: a\n'1': b\n",
    "inf key": ".inf: 1\n",
    "yes key": "yes: 1\n",
}


def key(k):
    """Render a mapping key the way json.dumps would put it on the wire.

    PyYAML happily builds a dict with non-string keys, and str() is not what a
    caller would ever have seen: json coerces True to "true" and None to "null",
    and spells infinity "Infinity".
    """
    if isinstance(k, str):
        return k
    if k is True:
        return "true"
    if k is False:
        return "false"
    if k is None:
        return "null"
    if isinstance(k, (int, float)):
        return json.dumps(k)
    return str(k)


def tag(v):
    """Type-tagged rendering, so a type change cannot compare equal to a value."""
    if v is None:
        return ["null", ""]
    if isinstance(v, bool):
        return ["bool", "true" if v else "false"]
    if isinstance(v, int):
        return ["int", str(v)]
    if isinstance(v, float):
        if math.isnan(v):
            return ["float", "nan"]
        if math.isinf(v):
            return ["float", "inf" if v > 0 else "-inf"]
        return ["float", repr(v)]
    if isinstance(v, str):
        return ["str", v]
    if isinstance(v, bytes):
        return ["binary", v.decode("utf-8", "replace")]
    if isinstance(v, datetime.datetime):
        return ["datetime", v.isoformat()]
    if isinstance(v, datetime.date):
        return ["date", v.isoformat()]
    if isinstance(v, dict):
        return ["map", {key(k): tag(x) for k, x in v.items()}]
    if isinstance(v, (list, tuple)):
        return ["list", [tag(x) for x in v]]
    if isinstance(v, (set, frozenset)):
        # Sorted so the pin does not depend on PYTHONHASHSEED.
        return ["set", [tag(x) for x in sorted(v, key=repr)]]
    return ["other:" + type(v).__name__, str(v)]


def main():
    out = {}
    with TemporaryDirectory() as d:
        path = Path(d) / "c.yml"
        for name, body in CASES.items():
            # Through the real loader, not yaml.safe_load: _read_yaml_mapping's
            # own except clause is part of what is being pinned, and so is the
            # `data if isinstance(data, dict) else {}` at the end of it.
            path.write_text(body, encoding="utf-8", newline="")
            try:
                out[name] = tag(config._read_yaml_mapping(path))
            except Exception as exc:
                # Three cases land here. _read_yaml_mapping catches only
                # yaml.YAMLError, so a constructor that raises KeyError or
                # ValueError escapes the loader that promises never to fail.
                out[name] = ["raises:" + type(exc).__name__, str(exc)]

    dest = ROOT / "testdata" / "pyyaml"
    dest.mkdir(parents=True, exist_ok=True)
    for stem, data in (("corpus", CASES), ("pin", out)):
        with (dest / f"{stem}.json").open("w", encoding="utf-8") as fh:
            json.dump(data, fh, indent=2, sort_keys=True, ensure_ascii=True)
            fh.write("\n")
    print(f"{len(out)} cases pinned to {dest}")


if __name__ == "__main__":
    main()
