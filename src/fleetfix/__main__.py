"""FleetFix entry point. `python -m fleetfix` and the `fleetfix` console script both land here."""

from __future__ import annotations

import argparse
import sys

from fleetfix import __version__


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

    from fleetfix.app import FleetFixApp

    FleetFixApp(read_only=args.read_only, target_user=args.target_user).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
