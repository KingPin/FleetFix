"""Unit tests for services.failed parser."""

from __future__ import annotations

from unittest.mock import MagicMock, patch

from fleetfix.modules.services.failed import (
    parse_failed_units,
    parse_show_user,
)

# ---------------------------------------------------------------------------
# Existing parse_failed_units tests (must remain passing)
# ---------------------------------------------------------------------------


def test_parse_failed_units_basic() -> None:
    text = (
        "myapp.service     loaded failed failed My Application Service\n"
        "other.service     loaded failed failed Other Daemon\n"
    )
    units = parse_failed_units(text)
    assert len(units) == 2
    assert units[0].name == "myapp.service"
    assert units[0].load == "loaded"
    assert units[0].active == "failed"
    assert units[0].sub == "failed"
    assert units[0].description == "My Application Service"


def test_parse_failed_units_handles_multi_word_description() -> None:
    text = "kafka.service loaded failed failed Apache Kafka brokers and topics\n"
    units = parse_failed_units(text)
    assert len(units) == 1
    assert units[0].description == "Apache Kafka brokers and topics"


def test_parse_failed_units_empty_input() -> None:
    assert parse_failed_units("") == []


def test_parse_failed_units_skips_short_rows() -> None:
    text = "too few cols\n"
    assert parse_failed_units(text) == []


# ---------------------------------------------------------------------------
# parse_show_user tests
# ---------------------------------------------------------------------------


def test_parse_show_user_basic() -> None:
    """Multi-block output with three units; empty User= defaults to root."""
    text = "User=root\n\nUser=appuser\n\nUser=\n"
    result = parse_show_user(text)
    assert result == ["root", "appuser", "root"]


def test_parse_show_user_empty() -> None:
    """Empty string returns empty list."""
    assert parse_show_user("") == []


def test_parse_show_user_block_missing_user_defaults_to_root() -> None:
    """Block with content but no `User=` line is defaulted to root."""
    text = "User=root\n\nOther=value\n\nUser=appuser\n"
    assert parse_show_user(text) == ["root", "root", "appuser"]


def test_parse_show_user_skips_trailing_empty_block() -> None:
    """Trailing `\\n\\n` should not produce a phantom entry."""
    text = "User=root\n\nUser=appuser\n\n"
    assert parse_show_user(text) == ["root", "appuser"]


# ---------------------------------------------------------------------------
# list_failed_units integration tests (subprocess mocked)
# ---------------------------------------------------------------------------

# Fixture: three units returned by the initial list-units call
_LIST_STDOUT = (
    "alpha.service  loaded failed failed Alpha Service\n"
    "beta.service   loaded failed failed Beta Service\n"
    "gamma.service  loaded failed failed Gamma Service\n"
)

# Bulk show output: alpha=root, beta=appuser, gamma= (empty → root)
_SHOW_STDOUT = "User=root\n\nUser=appuser\n\nUser=\n"


def _make_run(list_stdout: str, show_stdout: str, show_rc: int = 0) -> MagicMock:
    """Return a side_effect callable that dispatches on command args."""

    def _side_effect(cmd: list[str], **_kwargs: object) -> MagicMock:
        mock = MagicMock()
        if "list-units" in cmd:
            mock.returncode = 0
            mock.stdout = list_stdout
        else:
            # bulk show call
            mock.returncode = show_rc
            mock.stdout = show_stdout
        return mock

    return MagicMock(side_effect=_side_effect)


def test_list_failed_units_no_target_one_call() -> None:
    """Without target_user, exactly 1 subprocess call; all 3 units returned."""
    from fleetfix.modules.services.failed import list_failed_units

    mock_run = _make_run(_LIST_STDOUT, _SHOW_STDOUT)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units()

    assert len(units) == 3
    assert mock_run.call_count == 1


def test_list_failed_units_filters_to_target() -> None:
    """target_user='appuser' keeps only beta.service; exactly 2 subprocess calls."""
    from fleetfix.modules.services.failed import list_failed_units

    mock_run = _make_run(_LIST_STDOUT, _SHOW_STDOUT)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units(target_user="appuser")

    assert len(units) == 1
    assert units[0].name == "beta.service"
    assert mock_run.call_count == 2


def test_list_failed_units_target_root_includes_unspecified() -> None:
    """target_user='root' returns explicit-root AND empty-User units, not appuser."""
    from fleetfix.modules.services.failed import list_failed_units

    mock_run = _make_run(_LIST_STDOUT, _SHOW_STDOUT)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units(target_user="root")

    names = [u.name for u in units]
    assert "alpha.service" in names  # User=root
    assert "gamma.service" in names  # User= (empty → root)
    assert "beta.service" not in names  # User=appuser


def test_list_failed_units_show_failure_returns_empty() -> None:
    """Bulk show returning rc=1 causes list_failed_units to return []."""
    from fleetfix.modules.services.failed import list_failed_units

    mock_run = _make_run(_LIST_STDOUT, _SHOW_STDOUT, show_rc=1)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units(target_user="appuser")

    assert units == []


def test_list_failed_units_show_count_mismatch_returns_empty() -> None:
    """Bulk show returning only 2 blocks for 3 units causes [] to be returned."""
    from fleetfix.modules.services.failed import list_failed_units

    # Only 2 blocks in the show output instead of 3
    short_show = "User=root\n\nUser=appuser\n"
    mock_run = _make_run(_LIST_STDOUT, short_show)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units(target_user="appuser")

    assert units == []


def test_list_failed_units_target_with_zero_units_skips_show() -> None:
    """If initial list returns empty, no second subprocess call is made."""
    from fleetfix.modules.services.failed import list_failed_units

    mock_run = _make_run("", _SHOW_STDOUT)
    with patch("fleetfix.modules.services.failed.subprocess.run", mock_run):
        units = list_failed_units(target_user="appuser")

    assert units == []
    assert mock_run.call_count == 1
