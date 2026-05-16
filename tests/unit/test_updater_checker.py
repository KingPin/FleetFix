"""Unit tests for updater.checker — release parsing, semver compare, caching."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from fleetfix.updater.checker import (
    CACHE_TTL_S,
    DEFAULT_ASSET_NAME,
    ReleaseInfo,
    _cache_fresh,
    check_for_update,
    is_newer,
    parse_release,
)

ASSET = DEFAULT_ASSET_NAME


def _payload(tag: str, *, with_checksum: bool = True) -> dict[str, Any]:
    assets = [
        {"name": ASSET, "browser_download_url": f"https://example.com/{tag}/{ASSET}"},
    ]
    if with_checksum:
        assets.append(
            {
                "name": f"{ASSET}.sha256",
                "browser_download_url": f"https://example.com/{tag}/{ASSET}.sha256",
            }
        )
    return {
        "tag_name": tag,
        "html_url": f"https://example.com/release/{tag}",
        "body": "notes",
        "assets": assets,
    }


def test_is_newer_basic() -> None:
    assert is_newer("v0.2.0", "0.1.0") is True
    assert is_newer("0.2.0", "v0.1.0") is True
    assert is_newer("0.1.0", "0.1.0") is False
    assert is_newer("0.1.0", "0.2.0") is False


def test_is_newer_handles_bad_versions() -> None:
    assert is_newer("not-a-version", "0.1.0") is False
    assert is_newer("0.2.0", "not-a-version") is False


def test_parse_release_returns_release_info() -> None:
    info = parse_release(_payload("v0.2.0"), asset_name=ASSET)
    assert info is not None
    assert info.tag == "v0.2.0"
    assert info.version == "0.2.0"
    assert info.asset_url.endswith(ASSET)
    assert info.checksum_url.endswith(f"{ASSET}.sha256")


def test_parse_release_returns_none_without_checksum_asset() -> None:
    assert parse_release(_payload("v0.2.0", with_checksum=False), asset_name=ASSET) is None


def test_parse_release_returns_none_with_no_tag() -> None:
    assert parse_release({"assets": []}, asset_name=ASSET) is None


def test_parse_release_ignores_non_dict_assets() -> None:
    payload = _payload("v0.2.0")
    payload["assets"].append("garbage")  # type: ignore[arg-type]
    info = parse_release(payload, asset_name=ASSET)
    assert info is not None


def test_check_for_update_returns_release_when_newer(tmp_path: Path) -> None:
    cache = tmp_path / "release_check.json"
    info = check_for_update(
        "0.1.0",
        cache_path=cache,
        fetch=lambda url: _payload("v0.2.0"),
        now=1000.0,
    )
    assert info is not None
    assert info.tag == "v0.2.0"
    # Cache should have been written.
    cached = json.loads(cache.read_text())
    assert cached["payload"]["tag_name"] == "v0.2.0"
    assert cached["checked_at"] == 1000.0


def test_check_for_update_returns_none_when_same(tmp_path: Path) -> None:
    info = check_for_update(
        "0.2.0",
        cache_path=tmp_path / "c.json",
        fetch=lambda url: _payload("v0.2.0"),
        now=1.0,
    )
    assert info is None


def test_check_for_update_reads_fresh_cache(tmp_path: Path) -> None:
    cache = tmp_path / "c.json"
    cache.write_text(json.dumps({"checked_at": 100.0, "payload": _payload("v0.2.0")}))
    fetch_calls = {"count": 0}

    def fetch(url: str) -> dict[str, Any]:
        fetch_calls["count"] += 1
        return _payload("v0.3.0")

    info = check_for_update("0.1.0", cache_path=cache, fetch=fetch, now=200.0)
    assert info is not None
    # Got the cached v0.2.0, NOT the live v0.3.0.
    assert info.tag == "v0.2.0"
    assert fetch_calls["count"] == 0


def test_check_for_update_refetches_when_cache_stale(tmp_path: Path) -> None:
    cache = tmp_path / "c.json"
    cache.write_text(json.dumps({"checked_at": 100.0, "payload": _payload("v0.2.0")}))
    info = check_for_update(
        "0.1.0",
        cache_path=cache,
        fetch=lambda url: _payload("v0.3.0"),
        now=100.0 + CACHE_TTL_S + 1.0,
    )
    assert info is not None
    assert info.tag == "v0.3.0"


def test_check_for_update_swallows_network_errors(tmp_path: Path) -> None:
    def boom(url: str) -> dict[str, Any]:
        raise RuntimeError("network down")

    info = check_for_update("0.1.0", cache_path=tmp_path / "c.json", fetch=boom, now=1.0)
    assert info is None


def test_check_for_update_ignores_malformed_cache(tmp_path: Path) -> None:
    cache = tmp_path / "c.json"
    cache.write_text("not json")
    info = check_for_update(
        "0.1.0",
        cache_path=cache,
        fetch=lambda url: _payload("v0.2.0"),
        now=1.0,
    )
    assert info is not None


def test_cache_fresh_boundary() -> None:
    assert _cache_fresh(0.0, now=CACHE_TTL_S - 1, ttl_s=CACHE_TTL_S) is True
    assert _cache_fresh(0.0, now=CACHE_TTL_S + 1, ttl_s=CACHE_TTL_S) is False


@pytest.mark.parametrize("tag", ["", None, 123])
def test_parse_release_rejects_invalid_tag(tag: Any) -> None:
    assert parse_release({"tag_name": tag, "assets": []}, asset_name=ASSET) is None


def test_release_info_immutable() -> None:
    info = ReleaseInfo(
        tag="v0.2.0",
        version="0.2.0",
        asset_url="https://x",
        checksum_url="https://y",
        html_url="https://z",
        body="",
    )
    with pytest.raises(Exception):
        info.tag = "v0.3.0"  # type: ignore[misc]
