"""Unit tests for the storage tree scanner."""

from __future__ import annotations

from pathlib import Path

from fleetfix.modules.storage.tree import list_dir, summarize_subtree


def test_list_dir_returns_empty_for_missing(tmp_path: Path) -> None:
    assert list_dir(tmp_path / "nope") == []


def test_list_dir_returns_empty_for_file(tmp_path: Path) -> None:
    f = tmp_path / "file.txt"
    f.write_text("x")
    assert list_dir(f) == []


def test_list_dir_orders_dirs_before_files(tmp_path: Path) -> None:
    (tmp_path / "b_dir").mkdir()
    (tmp_path / "a_file.txt").write_text("x")
    (tmp_path / "c_dir").mkdir()
    names = [e.name for e in list_dir(tmp_path)]
    assert names == ["b_dir", "c_dir", "a_file.txt"]


def test_list_dir_populates_metadata(tmp_path: Path) -> None:
    f = tmp_path / "data.bin"
    f.write_bytes(b"x" * 256)
    entries = list_dir(tmp_path)
    [entry] = entries
    assert entry.name == "data.bin"
    assert entry.is_dir is False
    assert entry.is_symlink is False
    assert entry.size_bytes == 256
    assert entry.mtime_epoch > 0


def test_list_dir_marks_symlinks_not_as_dirs(tmp_path: Path) -> None:
    real = tmp_path / "real"
    real.mkdir()
    link = tmp_path / "shortcut"
    link.symlink_to(real)
    by_name = {e.name: e for e in list_dir(tmp_path)}
    assert by_name["shortcut"].is_symlink is True
    assert by_name["shortcut"].is_dir is False


def test_summarize_subtree_sums_recursively(tmp_path: Path) -> None:
    (tmp_path / "a.bin").write_bytes(b"x" * 100)
    sub = tmp_path / "sub"
    sub.mkdir()
    (sub / "b.bin").write_bytes(b"y" * 250)
    (sub / "deeper").mkdir()
    (sub / "deeper" / "c.bin").write_bytes(b"z" * 50)
    assert summarize_subtree(tmp_path) == 400


def test_summarize_subtree_empty_dir_is_zero(tmp_path: Path) -> None:
    assert summarize_subtree(tmp_path) == 0


def test_summarize_subtree_does_not_follow_symlinks(tmp_path: Path) -> None:
    real = tmp_path / "real"
    real.mkdir()
    (real / "big.bin").write_bytes(b"x" * 1000)
    inside = tmp_path / "tree"
    inside.mkdir()
    (inside / "small.bin").write_bytes(b"y" * 10)
    (inside / "loop").symlink_to(real)
    # only 10 should count — the symlink should not pull in `real/big.bin`
    assert summarize_subtree(inside) == 10
