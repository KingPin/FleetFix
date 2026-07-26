"""Run every case in ``testdata/cases.jsonl`` through the Python implementation.

This is the reference side of the differential harness: the Go port runs the same
manifest through the same fixtures and the two outputs are compared. The point of
doing it in one process is cost -- an interpreter start plus a ``fleetfix`` import
per case would put the harness well past the budget that lets it run on every PR,
and a harness that only runs nightly gets ignored and then muted.

Usage:
    python tools/oracle/py_oracle.py                 # all cases to stdout
    python tools/oracle/py_oracle.py --out py.jsonl
    python tools/oracle/py_oracle.py --case df.usage_mixed

Output is JSON-lines, one record per case, in manifest order:

    {"id": "df.usage_mixed", "value": [...]}
    {"id": "otel.malformed", "error": {"code": "YAMLError"}}

Two deliberate limits on what gets compared:

*Fields, not derived properties.* Dataclass ``@property`` accessors like
``DiskUsage.is_critical`` bake today's hardcoded severity constants into the
comparison, and those constants are exactly what moves into ``thresholds.yml``
during the port. Comparing them would make a planned change look like a
regression. The data is compared; the grading is not.

*Error codes, not messages.* A raising case records the exception class name only.
Python's message prose cannot and should not be reproduced in Go, so comparing it
would generate divergences that can never be fixed.

No case in the current corpus reads the clock or generates an identifier, so the
``FLEETFIX_FAKE_NOW`` / ``FLEETFIX_FAKE_UUID_SEED`` injection points the harness
design calls for have nothing to hook yet. They arrive with the audit-record cases,
which is the first thing here that will be time-dependent. Wiring them earlier
would mean two env vars no code reads, which is worse than not having them.
"""

from __future__ import annotations

import argparse
import dataclasses
import enum
import json
import math
import sys
import tempfile
from collections.abc import Callable
from datetime import date, datetime
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / "src"))

from fleetfix import config  # noqa: E402
from fleetfix.audit import logger as audit_logger  # noqa: E402
from fleetfix.audit import otel  # noqa: E402
from fleetfix.modules.disk import ghost, inodes, smart, usage  # noqa: E402
from fleetfix.modules.docker import dashboard as docker_dashboard  # noqa: E402
from fleetfix.modules.docker import hygiene as docker_hygiene  # noqa: E402
from fleetfix.modules.log_squeeze import gzip_inplace  # noqa: E402
from fleetfix.modules.network import (  # noqa: E402
    curl_probe,
    interfaces,
    ping,
    probes,
    resolver,
    sockets,
    tcp,
)
from fleetfix.modules.network import traceroute as tr  # noqa: E402
from fleetfix.modules.services import boot as services_boot  # noqa: E402
from fleetfix.modules.services import failed as services_failed  # noqa: E402
from fleetfix.modules.storage import env_check  # noqa: E402
from fleetfix.modules.system import metrics, thermal, updates  # noqa: E402
from fleetfix.updater import checker, installer  # noqa: E402

MANIFEST = REPO_ROOT / "testdata" / "cases.jsonl"
TESTDATA = REPO_ROOT / "testdata"

# The manifest's ``fn`` names are language-neutral on purpose: Go cannot dispatch
# on a Python dotted path, so each one needs a hand-written adapter on both sides.
# Every adapter takes (payload, args) -- payload is the fixture text for
# input="text" cases and a Path to a temp copy of it for input="path".
Adapter = Callable[[Any, dict[str, Any]], Any]


def materialise_tree(node: dict[str, Any], at: Path) -> None:
    """Write a JSON directory description out as a real tree.

    The fixture kind the readers that walk a directory need -- /sys/class/thermal,
    /sys/class/net, /proc/<pid> -- where a string is a file's contents and a nested
    object is a directory. One checked-in JSON file is a whole captured tree, so it
    keeps a checksum in the manifest like every other fixture.

    An empty object is an empty directory, which is a real sysfs shape: a driver
    that registered and then failed leaves one behind, and the reader must skip it
    for that reason rather than because it was not there at all.
    """
    at.mkdir(parents=True, exist_ok=True)
    for name, child in node.items():
        target = at / name
        if isinstance(child, str):
            target.write_text(child, encoding="utf-8")
        elif isinstance(child, dict):
            materialise_tree(child, target)
        else:
            raise TypeError(f"{target}: want a string (a file) or an object (a directory)")


DISPATCH: dict[str, Adapter] = {
    # system
    "system.parse_apt_upgradable": lambda t, a: updates.parse_apt_upgradable(t),
    "system.parse_notifier_text": lambda t, a: updates.parse_notifier_text(t),
    "system.read_uptime": lambda p, a: metrics.read_uptime(p),
    "system.read_loadavg": lambda p, a: metrics.read_loadavg(p),
    "system.read_meminfo": lambda p, a: metrics.read_meminfo(p),
    "system.read_zones": lambda p, a: thermal.read_zones(p),
    # Called through read_zones rather than over a hand-built list: the tie rule --
    # max() keeps its incumbent, so the lexicographically first of two equally warm
    # zones wins -- is only observable against the order read_zones produced.
    "system.hottest": lambda p, a: thermal.hottest(thermal.read_zones(p)),
    # disk
    "disk.parse_df": lambda t, a: usage.parse_df(t),
    "disk.parse_df_inodes": lambda t, a: inodes.parse_df_inodes(t),
    "disk.parse_lsof_field_output": lambda t, a: ghost.parse_lsof_field_output(t),
    "disk.parse_health": lambda t, a: smart.parse_health(t),
    "disk.parse_sata_attributes": lambda t, a: smart.parse_sata_attributes(t),
    "disk.parse_nvme_attributes": lambda t, a: smart.parse_nvme_attributes(t),
    # network
    "net.parse_curl_output": lambda t, a: curl_probe.parse_curl_output(a["url"], t),
    "net.parse_ping_output": lambda t, a: ping.parse_ping_output(a["target"], t),
    "net.parse_traceroute_output": lambda t, a: tr.parse_traceroute_output(
        a["target"], t, max_hops=a["max_hops"]
    ),
    "net.parse_tracepath_output": lambda t, a: tr.parse_tracepath_output(
        a["target"], t, max_hops=a["max_hops"]
    ),
    # One target per line: parse_host_port takes a single string, so the adapter
    # maps over the fixture's lines and the result is a list, with None wherever a
    # line named no reachable target.
    "net.parse_host_port": lambda t, a: [
        tcp.parse_host_port(line, default_port=a["default_port"]) for line in t.splitlines() if line
    ],
    "net.parse_resolv_conf": lambda t, a: resolver.parse_resolv_conf(t),
    "net.parse_ss_output": lambda t, a: sockets.parse_ss_output(t),
    "net.read_counters": lambda p, a: interfaces.read_counters(p),
    "net.default_route": lambda p, a: interfaces.default_route(p),
    "net.operstate": lambda p, a: interfaces.operstate(a["iface"], root=p),
    # Only the resolved Probes is compared. The clamp/reject warnings go to the
    # logger rather than the return value, so there is nothing here for the harness
    # to compare them against; the Go side pins them in a unit test instead.
    "net.load_probes": lambda p, a: probes.load_probes(path=p),
    # docker
    "docker.parse_reclaimed_total": lambda t, a: docker_hygiene.parse_reclaimed_total(t),
    "docker.parse_ps_json_lines": lambda t, a: docker_dashboard.parse_ps_json_lines(t),
    "docker.parse_inspect_fields": lambda t, a: docker_dashboard.parse_inspect_fields(t),
    "docker.parse_system_df_json_lines": lambda t, a: docker_hygiene.parse_system_df_json_lines(t),
    # services
    "services.parse_failed_units": lambda t, a: services_failed.parse_failed_units(t),
    "services.parse_show_user": lambda t, a: services_failed.parse_show_user(t),
    "services.parse_blame": lambda t, a: services_boot.parse_blame(t),
    # storage / log squeeze
    "storage.check_env_file": lambda p, a: env_check.check_env_file(
        p, required_keys=a.get("required_keys")
    ),
    "logsqueeze.lsof_has_writer": lambda t, a: gzip_inplace._lsof_has_writer(t),
    # config / audit / updater
    "config.read_paths_yaml": lambda p, a: config.read_paths_yaml(p),
    "config.read_perf_yaml": lambda p, a: config.read_perf_yaml(p),
    "config.read_probes_yaml": lambda p, a: config.read_probes_yaml(p),
    "audit.load_otel_config": lambda p, a: otel.load_otel_config(path=p, env=a["env"]),
    "audit.read_recent": lambda p, a: audit_logger.read_recent(p, limit=a["limit"]),
    "updater.parse_sha256_line": lambda t, a: installer.parse_sha256_line(
        t, asset_name=a["asset_name"]
    ),
    # json.loads here rather than a "json" input kind: the fixture is a captured API
    # response, and each side decoding it the way its own language does is part of
    # what the comparison is for -- an integer html_url renders through str() and the
    # two languages spell integers differently by default.
    "updater.parse_release": lambda t, a: checker.parse_release(
        json.loads(t), asset_name=a["asset_name"]
    ),
}


def plain(value: Any) -> Any:
    """Reduce a result to JSON types, deterministically.

    Tuples become lists and sets become sorted lists because neither survives
    JSON with its identity intact, and an arbitrary set order would show up as a
    divergence that is not one.
    """
    if isinstance(value, float) and not math.isfinite(value):
        # json.dumps writes the JavaScript-flavoured Infinity/NaN words, which Go's
        # decoder refuses outright -- so a `.inf` in a config file, which YAML 1.1
        # allows and probes.yml can therefore contain, would take the whole harness
        # down rather than being compared. Both sides spell a non-finite float as a
        # tagged string instead: still strict JSON, and a +inf that turns into a
        # -inf is still a divergence.
        if math.isnan(value):
            return "<nan>"
        return "<+inf>" if value > 0 else "<-inf>"
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, enum.Enum):
        return plain(value.value)
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {f.name: plain(getattr(value, f.name)) for f in dataclasses.fields(value)}
    if isinstance(value, Path):
        return str(value)
    if isinstance(value, (datetime, date)):
        return value.isoformat()
    if isinstance(value, dict):
        return {str(k): plain(v) for k, v in value.items()}
    if isinstance(value, (set, frozenset)):
        return sorted(plain(v) for v in value)
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    raise TypeError(f"no plain form for {type(value).__name__}")


def redact(value: Any, needle: str) -> Any:
    """Replace the temp input path wherever it surfaced in a result.

    ``EnvCheckResult`` carries the path it was handed, so without this the oracle
    emits a different value on every run and every case that touches it diverges
    for a reason that has nothing to do with parsing.
    """
    if isinstance(value, str):
        return "<input-path>" if value == needle else value
    if isinstance(value, dict):
        return {k: redact(v, needle) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v, needle) for v in value]
    return value


def load_cases() -> list[dict[str, Any]]:
    lines = MANIFEST.read_text(encoding="utf-8").splitlines()
    return [json.loads(line) for line in lines if line.strip()]


def run_case(case: dict[str, Any], tmp: Path) -> dict[str, Any]:
    fn = str(case["fn"])
    adapter = DISPATCH.get(fn)
    if adapter is None:
        return {"id": case["id"], "error": {"code": "UnknownFunction", "fn": fn}}

    data = (TESTDATA / str(case["fixture"])).read_bytes()
    args = dict(case["args"])
    needle = ""
    payload: Any
    if case["input"] == "path":
        # Keep the fixture's own basename: a reader debugging a failure can tell
        # which capture a temp path came from.
        target = tmp / Path(str(case["fixture"])).name
        target.write_bytes(data)
        payload = target
        needle = str(target)
    elif case["input"] == "tree":
        # One directory per case, not per fixture: two cases sharing a tree must not
        # be able to see each other's writes, even though nothing here writes today.
        root = tmp / str(case["id"]).replace("/", "_").replace("#", "-")
        materialise_tree(json.loads(data.decode()), root)
        payload = root
        needle = str(root)
    else:
        payload = data.decode()

    try:
        result = plain(adapter(payload, args))
    except Exception as exc:  # a raising case is a result to record, not a crash
        return {"id": case["id"], "error": {"code": type(exc).__name__}}
    return {"id": case["id"], "value": redact(result, needle) if needle else result}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, help="write here instead of stdout")
    parser.add_argument(
        "--case", action="append", dest="cases", help="run only this id (repeatable)"
    )
    parser.add_argument(
        "--list-functions",
        action="store_true",
        help="print the dispatchable function names and exit",
    )
    ns = parser.parse_args(argv)

    if ns.list_functions:
        print("\n".join(sorted(DISPATCH)))
        return 0

    cases = load_cases()
    if ns.cases:
        wanted = set(ns.cases)
        cases = [c for c in cases if c["id"] in wanted]
        if unknown := wanted - {str(c["id"]) for c in cases}:
            print(f"no such case: {sorted(unknown)}", file=sys.stderr)
            return 1

    with tempfile.TemporaryDirectory(prefix="fleetfix-oracle-") as tmpdir:
        tmp = Path(tmpdir)
        records = [run_case(case, tmp) for case in cases]

    body = "".join(json.dumps(r, sort_keys=True) + "\n" for r in records)
    if ns.out:
        ns.out.write_text(body, encoding="utf-8")
    else:
        sys.stdout.write(body)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
