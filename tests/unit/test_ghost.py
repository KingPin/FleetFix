"""Tests for `lsof +L1 -F` field-output parser."""

from __future__ import annotations

from fleetfix.modules.disk.ghost import GhostFile, parse_lsof_field_output, total_bytes

# lsof -F pcuLfsn +L1 produces one field per line. Tags:
#   p = pid (starts a process), c = command, u = user
#   f = fd (starts a file inside a process)
#   s = size, L = link count, n = name
_FIXTURE = """\
p812
csshd
uroot
f3
s4096
L1
n/var/log/auth.log
f4
s12345678
L0
n/var/log/journal/abc/system.journal (deleted)
p1234
cpostgres
upostgres
f9
s99999
L0
n/var/lib/postgresql/14/main/pg_log.deleted
"""


def test_parse_emits_only_deleted_files() -> None:
    files = parse_lsof_field_output(_FIXTURE)
    assert len(files) == 2


def test_parse_carries_process_metadata_across_files() -> None:
    files = parse_lsof_field_output(_FIXTURE)
    by_pid = {f.pid: f for f in files}
    assert by_pid[812].command == "sshd"
    assert by_pid[812].user == "root"
    assert by_pid[1234].command == "postgres"
    assert by_pid[1234].user == "postgres"


def test_parse_extracts_size_and_path() -> None:
    files = parse_lsof_field_output(_FIXTURE)
    deleted = next(f for f in files if f.pid == 812)
    assert deleted.size_bytes == 12345678
    assert "journal" in deleted.path
    assert deleted.fd == "4"


def test_total_bytes_sums_correctly() -> None:
    files = parse_lsof_field_output(_FIXTURE)
    assert total_bytes(files) == 12345678 + 99999


def test_parse_handles_empty_output() -> None:
    assert parse_lsof_field_output("") == []


def test_parse_handles_garbage_size() -> None:
    text = "p1\ncbad\nu0\nf0\nsoops\nL0\nn/tmp/x\n"
    files = parse_lsof_field_output(text)
    assert files == [GhostFile(pid=1, command="bad", user="0", fd="0", size_bytes=0, path="/tmp/x")]


def test_parse_ignores_held_files_with_nonzero_links() -> None:
    # All L=1; nothing should be returned.
    text = "p1\ncfoo\nuroot\nf0\ns100\nL1\nn/var/log/foo\nf1\ns200\nL2\nn/var/log/bar\n"
    assert parse_lsof_field_output(text) == []
