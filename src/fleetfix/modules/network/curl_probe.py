"""Probe a URL with curl and parse its timing breakdown.

We shell out to `curl` rather than using httpx so the timing numbers
match what techs already paste into tickets when they're debugging on
their own. The `-w` template emits a stable, parseable summary regardless
of TLS, redirects, or proxy hops.
"""

from __future__ import annotations

import subprocess
from dataclasses import dataclass

_CURL_FORMAT = (
    "FLEETFIX_CURL_PROBE\n"
    "http_code=%{http_code}\n"
    "time_namelookup=%{time_namelookup}\n"
    "time_connect=%{time_connect}\n"
    "time_appconnect=%{time_appconnect}\n"
    "time_starttransfer=%{time_starttransfer}\n"
    "time_total=%{time_total}\n"
    "size_download=%{size_download}\n"
)


@dataclass(frozen=True)
class CurlProbe:
    url: str
    ok: bool
    http_code: int
    time_total_s: float
    time_namelookup_s: float
    time_connect_s: float
    time_appconnect_s: float
    time_starttransfer_s: float
    size_download_bytes: int
    error: str | None = None


def parse_curl_output(url: str, output: str) -> CurlProbe | None:
    """Parse the -w template above into a CurlProbe. Returns None on bad input."""
    marker_idx = output.rfind("FLEETFIX_CURL_PROBE")
    if marker_idx == -1:
        return None
    tail = output[marker_idx:].splitlines()[1:]
    fields: dict[str, str] = {}
    for line in tail:
        if "=" not in line:
            continue
        key, _, value = line.partition("=")
        fields[key.strip()] = value.strip()
    try:
        http_code = int(fields["http_code"])
        return CurlProbe(
            url=url,
            ok=200 <= http_code < 400,
            http_code=http_code,
            time_namelookup_s=float(fields["time_namelookup"]),
            time_connect_s=float(fields["time_connect"]),
            time_appconnect_s=float(fields["time_appconnect"]),
            time_starttransfer_s=float(fields["time_starttransfer"]),
            time_total_s=float(fields["time_total"]),
            size_download_bytes=int(fields["size_download"]),
        )
    except (KeyError, ValueError):
        return None


def probe(
    url: str,
    *,
    timeout_s: int = 15,
    max_redirects: int = 5,
) -> CurlProbe:
    """Run curl against `url`. Always returns a CurlProbe — failures get error set."""
    args = [
        "curl",
        "-sS",
        "-o",
        "/dev/null",
        "--max-time",
        str(timeout_s),
        "--max-redirs",
        str(max_redirects),
        "-L",
        "-w",
        _CURL_FORMAT,
        url,
    ]
    try:
        result = subprocess.run(
            args,
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s + 2,
        )
    except subprocess.TimeoutExpired:
        return _failed(url, "subprocess timeout")
    except OSError as exc:
        return _failed(url, f"curl unavailable: {exc}")

    parsed = parse_curl_output(url, result.stdout)
    if parsed is not None:
        return parsed
    err = (result.stderr or "").strip().splitlines()
    return _failed(url, err[-1] if err else f"curl exited {result.returncode}")


def _failed(url: str, error: str) -> CurlProbe:
    return CurlProbe(
        url=url,
        ok=False,
        http_code=0,
        time_total_s=0.0,
        time_namelookup_s=0.0,
        time_connect_s=0.0,
        time_appconnect_s=0.0,
        time_starttransfer_s=0.0,
        size_download_bytes=0,
        error=error,
    )
