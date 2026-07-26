"""Tests for the /etc/resolv.conf reader."""

from __future__ import annotations

from pathlib import Path

from fleetfix.modules.network.resolver import parse_resolv_conf, read_resolver
from tests.support.fixtures import fixture

# What a systemd-resolved box actually ships.
_STUB = fixture("resolv_conf/systemd_stub.txt")

_CLASSIC = fixture("resolv_conf/classic.txt")

# Legacy `domain` form, plus a trailing comment on a directive line and a ';' comment.
_LEGACY_DOMAIN = fixture("resolv_conf/legacy_domain.txt")

_DOMAIN_THEN_SEARCH = fixture("resolv_conf/domain_then_search.txt")


def test_stub_resolver_is_flagged(tmp_path: Path) -> None:
    f = tmp_path / "resolv.conf"
    f.write_text(_STUB)
    cfg = read_resolver(source=f)
    assert cfg.nameservers == ("127.0.0.53",)
    assert cfg.stub_resolver is True
    assert cfg.search == (".",)
    assert cfg.options == ("edns0", "trust-ad")
    assert cfg.source == str(f)


def test_classic_multi_nameserver_preserves_order(tmp_path: Path) -> None:
    f = tmp_path / "resolv.conf"
    f.write_text(_CLASSIC)
    cfg = read_resolver(source=f)
    # Order is significant: the resolver asks these in file order.
    assert cfg.nameservers == ("10.0.0.53", "10.0.1.53", "1.1.1.1")
    assert cfg.search == ("corp.internal", "internal")
    assert cfg.options == ("timeout:2", "attempts:3")
    assert cfg.stub_resolver is False


def test_comments_stripped_and_legacy_domain_becomes_search() -> None:
    cfg = parse_resolv_conf(_LEGACY_DOMAIN)
    assert cfg.nameservers == ("10.0.0.53",)
    assert cfg.search == ("corp.internal",)


def test_last_search_directive_wins_over_domain() -> None:
    cfg = parse_resolv_conf(_DOMAIN_THEN_SEARCH)
    assert cfg.search == ("a.internal", "b.internal")


def test_stub_flag_is_false_when_stub_is_one_of_several() -> None:
    cfg = parse_resolv_conf(fixture("resolv_conf/stub_among_several.txt"))
    assert cfg.stub_resolver is False


def test_missing_file_yields_empty_config(tmp_path: Path) -> None:
    cfg = read_resolver(source=tmp_path / "nope.conf")
    assert cfg.nameservers == ()
    assert cfg.search == ()
    assert cfg.options == ()
    assert cfg.stub_resolver is False


def test_nameserver_without_address_is_ignored() -> None:
    cfg = parse_resolv_conf(fixture("resolv_conf/nameserver_without_address.txt"))
    assert cfg.nameservers == ("10.0.0.1",)
