"""FleetFix entry point. `python -m fleetfix` and the `fleetfix` console script both land here."""

from __future__ import annotations

import argparse
import os
import sys

from fleetfix import __version__


def _configure_animations(*, low_spec: bool) -> None:
    """Disable Textual animations on low-spec hosts, before Textual is imported.

    Textual reads ``TEXTUAL_ANIMATIONS`` once at import time, so this must run
    before ``fleetfix.app`` (which imports Textual) is loaded. Precedence:
    an explicit operator ``TEXTUAL_ANIMATIONS`` env var always wins; then the
    ``--low-spec`` flag; then ``perf.yml`` / the single-core auto-detect.
    """
    if "TEXTUAL_ANIMATIONS" in os.environ:
        return
    from fleetfix.config import (
        PERF_CONFIG_PATH,
        read_perf_yaml,
        resolve_reduce_animations,
    )

    reduce = low_spec or resolve_reduce_animations(
        perf_cfg=read_perf_yaml(PERF_CONFIG_PATH),
        cpu_count=os.cpu_count() or 1,
    )
    if reduce:
        os.environ["TEXTUAL_ANIMATIONS"] = "none"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="fleetfix",
        description="Terminal-UI triage toolbox for Ubuntu/Debian fleet operators.",
    )
    parser.add_argument(
        "--version",
        action="version",
        version=f"fleetfix {__version__}",
    )
    parser.add_argument(
        "--read-only",
        action="store_true",
        help="Disable every destructive action (training / shadow mode).",
    )
    parser.add_argument(
        "--update",
        action="store_true",
        help="Check for and install the latest release, then exit (no TUI).",
    )
    parser.add_argument(
        "--force",
        "-y",
        action="store_true",
        help="With --update: skip the confirmation prompt (for ansible/CI).",
    )
    parser.add_argument(
        "--low-spec",
        action="store_true",
        help=(
            "Disable UI animations for anemic/single-core hosts. Auto-enabled "
            "on single-core CPUs; also settable via ~/.config/fleetfix/perf.yml "
            "(reduce_animations) or the TEXTUAL_ANIMATIONS env var."
        ),
    )
    parser.add_argument(
        "--target-user",
        default=None,
        help=(
            "Inspect this user's footprint (home, units) instead of the "
            "invoking user's. Pass an empty string to force-clear a "
            "target_user set in paths.yml."
        ),
    )
    args = parser.parse_args(argv)

    if args.update:
        from fleetfix.updater.cli import run_update

        return run_update(force=args.force)

    _configure_animations(low_spec=args.low_spec)

    from fleetfix.app import FleetFixApp

    FleetFixApp(read_only=args.read_only, target_user=args.target_user).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
