"""`docker system df` summary + image / volume prune wrappers.

`docker system df --format '{{json .}}'` emits one JSON line per category
(Images, Containers, Local Volumes, Build Cache) with a `Reclaimable` field
in the form "45.68GB (85%)". We parse the byte count and the percentage
separately so the UI can show both.

Prune actions go through `docker image prune -f` / `docker volume prune -f`.
The Docker CLI doesn't expose a structured `--json` mode for these, so we
parse the "Total reclaimed space: <size>" trailer ourselves. Each prune is
audit-wrapped.
"""

from __future__ import annotations

import json
import re
import subprocess
from dataclasses import dataclass

from fleetfix.audit.logger import AuditLogger

_RECLAIMABLE_RE = re.compile(r"^\s*(?P<size>[0-9.]+\s*[A-Za-z]*B?)\s*(?:\((?P<pct>\d+)%\))?\s*$")
_RECLAIMED_RE = re.compile(r"Total reclaimed space:\s*([0-9.]+\s*[A-Za-z]*B?)", re.IGNORECASE)


@dataclass(frozen=True)
class DfRow:
    type: str
    total_count: int
    active: int
    size_bytes: int
    reclaimable_bytes: int
    reclaimable_pct: int


@dataclass(frozen=True)
class PruneResult:
    target: str  # "images" | "volumes"
    bytes_reclaimed: int
    ok: bool
    error: str | None = None


def parse_system_df_json_lines(text: str) -> list[DfRow]:
    rows: list[DfRow] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        rows.append(
            DfRow(
                type=obj.get("Type", ""),
                total_count=_int_or_zero(obj.get("TotalCount")),
                active=_int_or_zero(obj.get("Active")),
                size_bytes=parse_size(obj.get("Size", "")),
                reclaimable_bytes=parse_reclaimable_bytes(obj.get("Reclaimable", "")),
                reclaimable_pct=parse_reclaimable_pct(obj.get("Reclaimable", "")),
            )
        )
    return rows


def parse_reclaimable_bytes(reclaimable: str) -> int:
    m = _RECLAIMABLE_RE.match(reclaimable or "")
    if not m:
        return 0
    return parse_size(m.group("size"))


def parse_reclaimable_pct(reclaimable: str) -> int:
    m = _RECLAIMABLE_RE.match(reclaimable or "")
    if not m or m.group("pct") is None:
        return 0
    return int(m.group("pct"))


def parse_size(value: str) -> int:
    """Parse Docker's human-readable size (e.g. '45.68GB', '136B', '0 B')."""
    value = (value or "").strip().replace(" ", "")
    if not value:
        return 0
    m = re.match(r"^([0-9.]+)([A-Za-z]*B?)$", value)
    if not m:
        return 0
    try:
        number = float(m.group(1))
    except ValueError:
        return 0
    unit = m.group(2).upper()
    multipliers = {
        "B": 1,
        "": 1,
        "KB": 1000,
        "MB": 1000**2,
        "GB": 1000**3,
        "TB": 1000**4,
        "KIB": 1024,
        "MIB": 1024**2,
        "GIB": 1024**3,
        "TIB": 1024**4,
    }
    return int(number * multipliers.get(unit, 1))


def parse_reclaimed_total(text: str) -> int:
    m = _RECLAIMED_RE.search(text or "")
    if not m:
        return 0
    return parse_size(m.group(1))


def system_df() -> list[DfRow]:
    try:
        result = subprocess.run(
            ["docker", "system", "df", "--format", "{{json .}}"],
            capture_output=True,
            text=True,
            timeout=10,
            check=False,
        )
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return []
    if result.returncode != 0:
        return []
    return parse_system_df_json_lines(result.stdout)


def prune_images(*, audit: AuditLogger, include_all: bool = False) -> PruneResult:
    """Run `docker image prune -f`. When include_all is True, adds -a (untagged + unused)."""
    args = ["docker", "image", "prune", "-f"]
    if include_all:
        args.append("-a")
    return _run_prune("images", args, audit=audit, action="docker.prune_images")


def prune_volumes(*, audit: AuditLogger) -> PruneResult:
    return _run_prune(
        "volumes",
        ["docker", "volume", "prune", "-f"],
        audit=audit,
        action="docker.prune_volumes",
    )


def _run_prune(
    target: str,
    argv: list[str],
    *,
    audit: AuditLogger,
    action: str,
) -> PruneResult:
    with audit.action(action, target={"target": target, "argv": argv}) as call:
        error, reclaimed = _invoke_prune(argv)
        if error is not None:
            call.set_result(ok=False, error=error)
            return PruneResult(target=target, bytes_reclaimed=0, ok=False, error=error)
        call.set_result(bytes_reclaimed=reclaimed)
    return PruneResult(target=target, bytes_reclaimed=reclaimed, ok=True)


def _invoke_prune(argv: list[str]) -> tuple[str | None, int]:
    try:
        result = subprocess.run(
            argv,
            capture_output=True,
            text=True,
            timeout=120,
            check=True,
        )
    except FileNotFoundError:
        return "docker not found", 0
    except subprocess.TimeoutExpired:
        return "prune timed out", 0
    except subprocess.CalledProcessError as exc:
        return ((exc.stderr or "").strip() or "prune failed"), 0
    return None, parse_reclaimed_total(result.stdout)


def _int_or_zero(value: object) -> int:
    try:
        return int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return 0
