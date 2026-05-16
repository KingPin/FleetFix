"""Validate a `.env` (or any `key=value`) file.

Triage flow: when an app on the box is misbehaving, the first question
is usually "is the env file present and well-formed?". This module gives
the storage screen a fast, deterministic answer.

Doesn't enforce a schema beyond "configurable required keys" — the
fleet-wide policy of which keys must be present lives in the per-host
profile we add in milestone 10.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path

_LINE_RE = re.compile(r"""^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=(.*)$""")


@dataclass(frozen=True)
class EnvIssue:
    line_no: int
    raw: str
    message: str


@dataclass
class EnvCheckResult:
    path: Path
    exists: bool
    readable: bool
    keys: dict[str, str] = field(default_factory=dict)
    missing_required: list[str] = field(default_factory=list)
    issues: list[EnvIssue] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.exists and self.readable and not self.missing_required and not self.issues


def check_env_file(path: Path, *, required_keys: list[str] | None = None) -> EnvCheckResult:
    """Parse `path` as a dotenv file and report what's missing or malformed.

    Comments (`#` to end of line) and blank lines are ignored. Lines that
    don't match `KEY=value` become issues but don't abort the scan — the
    operator wants to see *all* problems at once.
    """
    required_keys = required_keys or []
    result = EnvCheckResult(path=path, exists=path.exists(), readable=False)

    if not result.exists:
        result.missing_required = list(required_keys)
        return result

    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as exc:
        result.issues.append(EnvIssue(line_no=0, raw="", message=f"unable to read file: {exc}"))
        return result

    result.readable = True
    for line_no, raw in enumerate(text.splitlines(), start=1):
        stripped = raw.split("#", 1)[0].rstrip()
        if not stripped.strip():
            continue
        match = _LINE_RE.match(stripped)
        if not match:
            result.issues.append(
                EnvIssue(line_no=line_no, raw=raw, message="not in KEY=value form")
            )
            continue
        key, value = match.group(1), match.group(2).strip()
        value = _strip_quotes(value)
        if key in result.keys:
            result.issues.append(
                EnvIssue(line_no=line_no, raw=raw, message=f"duplicate key: {key}")
            )
        result.keys[key] = value

    result.missing_required = [k for k in required_keys if k not in result.keys]
    return result


def _strip_quotes(value: str) -> str:
    if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
        return value[1:-1]
    return value
