"""Release checker — talks to the GitHub Releases API.

Flow on launch:
  1. Try cache (``~/.cache/fleetfix/release_check.json``); if <1h old,
     reuse it. The cache stops a relaunch-loop from hammering GitHub.
  2. Otherwise GET ``GITHUB_RELEASES_URL``, pick the asset matching
     ``fleetfix-linux-x86_64`` and its ``.sha256`` companion.
  3. If the remote tag parses as semver and is newer than the local
     ``__version__``, return a ``ReleaseInfo``. Otherwise return None.

Failures (no network, rate limiting, invalid JSON) silently return None
— a tech mid-triage shouldn't see a spurious "updater broken" warning.
"""

from __future__ import annotations

import json
import logging
import time
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import httpx
from packaging.version import InvalidVersion, Version

from fleetfix.config import GITHUB_RELEASES_URL, USER_CACHE_DIR

_log = logging.getLogger(__name__)

CACHE_PATH = USER_CACHE_DIR / "release_check.json"
CACHE_TTL_S = 3_600  # 1 hour
DEFAULT_ASSET_NAME = "fleetfix-linux-x86_64"
HTTP_TIMEOUT_S = 5.0


@dataclass(frozen=True)
class ReleaseInfo:
    tag: str
    version: str
    asset_url: str
    checksum_url: str
    html_url: str
    body: str


Fetcher = Callable[[str], dict[str, Any]]


def _default_fetch(url: str) -> dict[str, Any]:
    headers = {"Accept": "application/vnd.github+json"}
    with httpx.Client(timeout=HTTP_TIMEOUT_S, follow_redirects=True) as client:
        resp = client.get(url, headers=headers)
    resp.raise_for_status()
    data = resp.json()
    if not isinstance(data, dict):
        raise ValueError("releases API returned non-object")
    return data


def _strip_v(tag: str) -> str:
    return tag[1:] if tag.startswith("v") else tag


def is_newer(remote: str, local: str) -> bool:
    """True if the remote semver is strictly greater than the local one."""
    try:
        return Version(_strip_v(remote)) > Version(_strip_v(local))
    except InvalidVersion:
        return False


def parse_release(payload: dict[str, Any], *, asset_name: str) -> ReleaseInfo | None:
    """Extract the binary + checksum asset URLs from a GitHub release payload."""
    tag = payload.get("tag_name")
    if not isinstance(tag, str) or not tag:
        return None
    assets = payload.get("assets") or []
    if not isinstance(assets, list):
        return None
    asset_url: str | None = None
    checksum_url: str | None = None
    for a in assets:
        if not isinstance(a, dict):
            continue
        name = a.get("name")
        url = a.get("browser_download_url")
        if not isinstance(name, str) or not isinstance(url, str):
            continue
        if name == asset_name:
            asset_url = url
        elif name == f"{asset_name}.sha256":
            checksum_url = url
    if not asset_url or not checksum_url:
        return None
    return ReleaseInfo(
        tag=tag,
        version=_strip_v(tag),
        asset_url=asset_url,
        checksum_url=checksum_url,
        html_url=str(payload.get("html_url") or ""),
        body=str(payload.get("body") or ""),
    )


def _read_cache(path: Path) -> dict[str, Any] | None:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        return None
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict):
        return None
    return data


def _cache_fresh(cached_at: float, *, now: float, ttl_s: int) -> bool:
    return (now - cached_at) < ttl_s


def _write_cache(path: Path, *, payload: dict[str, Any], now: float) -> None:
    try:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(
            json.dumps({"checked_at": now, "payload": payload}),
            encoding="utf-8",
        )
    except OSError:
        _log.debug("could not write release-check cache to %s", path, exc_info=True)


def check_for_update(
    current_version: str,
    *,
    asset_name: str = DEFAULT_ASSET_NAME,
    cache_path: Path | None = None,
    cache_ttl_s: int = CACHE_TTL_S,
    fetch: Fetcher | None = None,
    url: str = GITHUB_RELEASES_URL,
    now: float | None = None,
) -> ReleaseInfo | None:
    """Return a newer release if one exists, otherwise None.

    A failure to reach GitHub is silent — the caller treats absence the
    same way as "no update available".
    """
    cache_path = cache_path if cache_path is not None else CACHE_PATH
    now_s = now if now is not None else time.time()
    cached = _read_cache(cache_path)
    payload: dict[str, Any] | None = None
    if cached is not None:
        cached_at = cached.get("checked_at")
        if isinstance(cached_at, (int, float)) and _cache_fresh(
            float(cached_at), now=now_s, ttl_s=cache_ttl_s
        ):
            cached_payload = cached.get("payload")
            if isinstance(cached_payload, dict):
                payload = cached_payload

    if payload is None:
        fetcher = fetch or _default_fetch
        try:
            payload = fetcher(url)
        except Exception:
            _log.debug("release check failed; treating as no update", exc_info=True)
            return None
        _write_cache(cache_path, payload=payload, now=now_s)

    release = parse_release(payload, asset_name=asset_name)
    if release is None:
        return None
    if not is_newer(release.version, current_version):
        return None
    return release
