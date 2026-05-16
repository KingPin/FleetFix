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
        "--json",
        choices=["health"],
        help="Emit a structured report to stdout and exit (no TUI).",
    )
    args = parser.parse_args(argv)

    if args.json == "health":
        from fleetfix.cli import json_health

        return json_health.run()

    from fleetfix.app import FleetFixApp

    FleetFixApp(read_only=args.read_only).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
