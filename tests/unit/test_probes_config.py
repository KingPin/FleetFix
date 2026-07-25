"""Tests for probes.yml loading, merging, and clamping."""

from __future__ import annotations

from pathlib import Path

from fleetfix.config import read_probes_yaml
from fleetfix.modules.network.probes import (
    DEFAULT_PROBES,
    load_probes,
    resolve_probes,
)
from fleetfix.modules.network.tcp import TcpTarget

# The example config the README documents, round-tripped end to end.
_EXAMPLE = """\
ping:
  targets: [10.0.0.1, 8.8.8.8]
  count: 5
  interval_s: 0.3
dns:
  names: [db.corp.internal, github.com]
http:
  urls: [https://api.corp.internal/health]
tcp:
  targets: ["db.corp.internal:5432", "https://api.corp.internal"]
traceroute:
  max_hops: 20
ladder:
  internet_target: 9.9.9.9
  dns_name: db.corp.internal
  https_url: https://api.corp.internal/health
"""


def test_no_config_yields_the_built_in_defaults() -> None:
    # A freshly imaged host with no probes.yml must be fully functional.
    assert resolve_probes(probes_cfg={}) == DEFAULT_PROBES
    assert resolve_probes(probes_cfg=None) == DEFAULT_PROBES


def test_scalar_override_leaves_its_siblings_alone() -> None:
    probes = resolve_probes(probes_cfg={"ping": {"count": 3}})
    assert probes.ping.count == 3
    # "Shorter ping", not "also zero the interval".
    assert probes.ping.interval_s == DEFAULT_PROBES.ping.interval_s
    assert probes.ping.timeout_s == DEFAULT_PROBES.ping.timeout_s
    assert probes.ping.targets == DEFAULT_PROBES.ping.targets


def test_target_list_replaces_wholesale() -> None:
    probes = resolve_probes(probes_cfg={"ping": {"targets": ["10.0.0.1"]}})
    # Not appended: a permanently-red public target on an egress-filtered host
    # trains operators to ignore red.
    assert probes.ping.targets == ("10.0.0.1",)


def test_empty_list_is_an_explicit_no_presets() -> None:
    probes = resolve_probes(probes_cfg={"dns": {"names": []}})
    assert probes.dns.names == ()


def test_junk_scalars_fall_back_to_defaults() -> None:
    probes = resolve_probes(probes_cfg={"ping": {"count": "banana", "interval_s": True}})
    assert probes.ping.count == DEFAULT_PROBES.ping.count
    # bool is an int subclass, so `interval_s: true` must not become 1.0.
    assert probes.ping.interval_s == DEFAULT_PROBES.ping.interval_s


def test_junk_list_falls_back_to_defaults() -> None:
    probes = resolve_probes(probes_cfg={"ping": {"targets": "8.8.8.8"}})
    assert probes.ping.targets == DEFAULT_PROBES.ping.targets


def test_out_of_range_numbers_are_clamped() -> None:
    probes = resolve_probes(
        probes_cfg={
            "ping": {"count": 100000, "interval_s": 0, "timeout_s": 99999},
            "traceroute": {"max_hops": 500, "queries": 99, "wait_s": 0},
            "http": {"max_redirects": -5},
        }
    )
    # These become subprocess arguments; count=100000 at interval 0 is a
    # self-inflicted DoS on the operator's own box.
    assert probes.ping.count == 900
    assert probes.ping.interval_s == 0.05
    assert probes.ping.timeout_s == 300
    assert probes.traceroute.max_hops == 30
    assert probes.traceroute.queries == 3
    assert probes.traceroute.wait_s == 1
    assert probes.http.max_redirects == 0


def test_non_mapping_section_is_ignored_whole() -> None:
    probes = resolve_probes(probes_cfg={"ping": ["8.8.8.8"], "dns": "github.com"})
    assert probes.ping == DEFAULT_PROBES.ping
    assert probes.dns == DEFAULT_PROBES.dns


def test_tcp_targets_drop_entries_with_no_determinable_port() -> None:
    probes = resolve_probes(
        probes_cfg={"tcp": {"targets": ["db.internal:5432", "garbage", "x:99999"]}}
    )
    # "garbage" has no port and we refuse to guess one; x:99999 is out of range.
    assert probes.tcp.targets == (TcpTarget("db.internal", 5432),)


def test_tcp_target_accepts_a_url() -> None:
    probes = resolve_probes(probes_cfg={"tcp": {"targets": ["https://api.internal/health"]}})
    assert probes.tcp.targets == (TcpTarget("api.internal", 443),)


def test_ladder_strings_override_individually() -> None:
    probes = resolve_probes(probes_cfg={"ladder": {"dns_name": "db.corp.internal"}})
    assert probes.ladder.dns_name == "db.corp.internal"
    assert probes.ladder.internet_target == DEFAULT_PROBES.ladder.internet_target
    assert probes.ladder.https_url == DEFAULT_PROBES.ladder.https_url


def test_blank_ladder_string_falls_back() -> None:
    probes = resolve_probes(probes_cfg={"ladder": {"internet_target": "   "}})
    assert probes.ladder.internet_target == DEFAULT_PROBES.ladder.internet_target


def test_missing_file_reads_as_empty(tmp_path: Path) -> None:
    assert read_probes_yaml(tmp_path / "nope.yml") == {}


def test_invalid_yaml_reads_as_empty(tmp_path: Path) -> None:
    path = tmp_path / "probes.yml"
    path.write_text("ping: [unclosed\n")
    assert read_probes_yaml(path) == {}


def test_top_level_list_reads_as_empty(tmp_path: Path) -> None:
    path = tmp_path / "probes.yml"
    path.write_text("- 8.8.8.8\n- 1.1.1.1\n")
    assert read_probes_yaml(path) == {}


def test_load_probes_never_raises_on_a_broken_file(tmp_path: Path) -> None:
    path = tmp_path / "probes.yml"
    path.write_text("ping: {count: banana\n")
    # A broken config must never be why the Network screen won't open.
    assert load_probes(path=path) == DEFAULT_PROBES


def test_load_probes_round_trips_the_documented_example(tmp_path: Path) -> None:
    path = tmp_path / "probes.yml"
    path.write_text(_EXAMPLE)
    probes = load_probes(path=path)
    assert probes.ping.targets == ("10.0.0.1", "8.8.8.8")
    assert probes.ping.count == 5
    assert probes.ping.interval_s == 0.3
    assert probes.ping.timeout_s == DEFAULT_PROBES.ping.timeout_s
    assert probes.dns.names == ("db.corp.internal", "github.com")
    assert probes.http.urls == ("https://api.corp.internal/health",)
    assert probes.tcp.targets == (
        TcpTarget("db.corp.internal", 5432),
        TcpTarget("api.corp.internal", 443),
    )
    assert probes.traceroute.max_hops == 20
    assert probes.traceroute.queries == DEFAULT_PROBES.traceroute.queries
    assert probes.ladder.internet_target == "9.9.9.9"
    assert probes.ladder.dns_name == "db.corp.internal"
    assert probes.ladder.https_url == "https://api.corp.internal/health"
