"""Unit tests for the system-path blacklist."""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.modules.disk.blacklist import (
    BlacklistedPath,
    is_blacklisted,
    refuse_if_blacklisted,
)


@pytest.mark.parametrize(
    "target",
    [
        "/etc/passwd",
        "/etc",
        "/boot/grub/grub.cfg",
        "/usr/lib/python3.10",
        "/lib64/ld-linux-x86-64.so.2",
        "/sbin/init",
        "/proc/1/status",
        "/sys/class/net",
        "/dev/null",
        "/var/lib/dpkg/status",
        "/var/lib/apt/lists",
    ],
)
def test_system_paths_are_blacklisted(target: str) -> None:
    assert is_blacklisted(Path(target)) is not None


@pytest.mark.parametrize(
    "target",
    [
        "/home/appuser/dump.sql",
        "/tmp/staging.log",
        "/opt/appdata/run",
        "/var/log/app.log",  # /var/log is fine — only /var/lib/{dpkg,apt} are blacklisted
        "/etcetera/notes",  # prefix-similar but not under /etc
    ],
)
def test_user_paths_are_allowed(target: str) -> None:
    assert is_blacklisted(Path(target)) is None


def test_etc_prod_not_matched_as_etc() -> None:
    """`/etc-prod/x` must NOT match the `/etc` blacklist — parts comparison only."""
    assert is_blacklisted(Path("/etc-prod/secret")) is None


def test_root_directory_itself_is_blacklisted() -> None:
    assert is_blacklisted(Path("/")) is not None


def test_symlink_to_blacklisted_path_is_caught(tmp_path: Path) -> None:
    link = tmp_path / "innocent"
    link.symlink_to("/etc")
    assert is_blacklisted(link) is not None


def test_parent_traversal_is_resolved() -> None:
    """A path traversal that lands in /etc must still be caught."""
    sneaky = Path("/tmp") / ".." / "etc" / "passwd"
    assert is_blacklisted(sneaky) is not None


def test_extra_blacklist_is_honoured() -> None:
    assert is_blacklisted(Path("/srv/critical/file"), extra=("/srv/critical",)) is not None


def test_refuse_raises_with_match_info() -> None:
    with pytest.raises(BlacklistedPath) as exc_info:
        refuse_if_blacklisted(Path("/etc/passwd"))
    assert exc_info.value.target == Path("/etc/passwd")
    assert exc_info.value.matched == "/etc"


def test_refuse_passes_through_safe_paths(tmp_path: Path) -> None:
    refuse_if_blacklisted(tmp_path / "harmless.txt")  # should not raise


def test_symlink_chain_to_blacklisted_path_is_caught(tmp_path: Path) -> None:
    """A multi-hop symlink chain must still resolve through to the real target."""
    final_target = "/etc"
    link_b = tmp_path / "b"
    link_b.symlink_to(final_target)
    link_a = tmp_path / "a"
    link_a.symlink_to(link_b)
    assert is_blacklisted(link_a) is not None


def test_symlink_loop_does_not_crash(tmp_path: Path) -> None:
    """Self-referential symlinks must be handled without raising."""
    loop = tmp_path / "loop"
    loop.symlink_to(loop)
    # We don't care whether it's blacklisted — only that the lookup doesn't
    # propagate the OSError/RuntimeError from .resolve().
    is_blacklisted(loop)


def test_broken_symlink_resolved_to_target_path(tmp_path: Path) -> None:
    """Broken symlinks resolve to their (non-existent) target — still subject to blacklist."""
    broken = tmp_path / "broken"
    broken.symlink_to("/etc/this-does-not-exist")
    assert is_blacklisted(broken) is not None


def test_nested_symlink_pointing_under_blacklist(tmp_path: Path) -> None:
    """A symlink that lands deep inside a blacklisted tree is caught."""
    link = tmp_path / "log_link"
    link.symlink_to("/var/lib/dpkg/status")
    assert is_blacklisted(link) is not None


def test_relative_target_resolved_against_cwd(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A bare relative path must be resolved before being matched."""
    # Resolve relative paths against an /etc-rooted CWD via monkeypatch.
    monkeypatch.chdir("/etc")
    assert is_blacklisted(Path("./passwd")) is not None
