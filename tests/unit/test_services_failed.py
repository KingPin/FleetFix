"""Unit tests for services.failed parser."""

from __future__ import annotations

from fleetfix.modules.services.failed import parse_failed_units


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
