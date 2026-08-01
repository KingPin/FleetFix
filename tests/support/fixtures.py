"""Load captured command output from ``testdata/``.

These files are the shared corpus: the same bytes feed the Python parsers here
and, from the Go port onward, the Go ones. That only works if both sides read
them identically, so this loader is deliberately dumb — read the bytes, decode
UTF-8, hand back the string.

Never open a fixture in text mode. Python's universal-newline translation would
silently rewrite ``\\r\\n`` on the way in, and some of these captures exist
precisely to pin down how a parser handles odd line endings and trailing
whitespace. A fixture that changes as it is read is not a fixture.
"""

from __future__ import annotations

from functools import cache
from pathlib import Path

TESTDATA = Path(__file__).resolve().parents[2] / "testdata"


@cache
def fixture(rel: str) -> str:
    """Return the fixture at ``rel``, e.g. ``fixture("df/usage_mixed.txt")``."""
    path = TESTDATA / rel
    if not path.is_file():
        raise FileNotFoundError(f"no fixture at {path} (relative to {TESTDATA})")
    return path.read_bytes().decode()


def fixture_bytes(rel: str) -> bytes:
    """Raw bytes, for the manifest's checksums and for non-UTF-8 captures."""
    return (TESTDATA / rel).read_bytes()
