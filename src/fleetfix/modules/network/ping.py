"""Run `ping` against a target and parse the summary line into a struct.

We invoke iputils-ping (the default on Ubuntu/Debian) and parse two
sections of the output:

    1. The "rtt min/avg/max/mdev = X/X/X/X ms" line
    2. The "N packets transmitted, M received, P% packet loss" line

We keep this in its own module so the tests can feed in canned output
without ever touching the network.
"""

from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass

_RTT_RE = re.compile(
    r"rtt min/avg/max/mdev = "
    r"([0-9.]+)/([0-9.]+)/([0-9.]+)/([0-9.]+)\s*ms"
)
_LOSS_RE = re.compile(
    r"(\d+) packets transmitted, (\d+) (?:received|packets received), "
    r"(?:[\+\-]?\d+ errors, )?([\d.]+)% packet loss"
)


@dataclass(frozen=True)
class PingSummary:
    target: str
    sent: int
    received: int
    loss_pct: float
    rtt_min_ms: float
    rtt_avg_ms: float
    rtt_max_ms: float
    rtt_mdev_ms: float

    @property
    def jitter_ms(self) -> float:
        """Alias for mdev — the conventional name for network jitter."""
        return self.rtt_mdev_ms


def parse_ping_output(target: str, output: str) -> PingSummary | None:
    """Parse iputils `ping` summary. Returns None if the summary isn't present."""
    loss_match = _LOSS_RE.search(output)
    if loss_match is None:
        return None
    sent = int(loss_match.group(1))
    received = int(loss_match.group(2))
    loss = float(loss_match.group(3))

    rtt_match = _RTT_RE.search(output)
    if rtt_match is None:
        # 100% loss case: no rtt line. Synthesize an all-zero rtt block.
        return PingSummary(
            target=target,
            sent=sent,
            received=received,
            loss_pct=loss,
            rtt_min_ms=0.0,
            rtt_avg_ms=0.0,
            rtt_max_ms=0.0,
            rtt_mdev_ms=0.0,
        )

    return PingSummary(
        target=target,
        sent=sent,
        received=received,
        loss_pct=loss,
        rtt_min_ms=float(rtt_match.group(1)),
        rtt_avg_ms=float(rtt_match.group(2)),
        rtt_max_ms=float(rtt_match.group(3)),
        rtt_mdev_ms=float(rtt_match.group(4)),
    )


def run_ping(
    target: str,
    *,
    count: int = 60,
    interval_s: float = 0.2,
    timeout_s: int = 30,
) -> PingSummary | None:
    """Spawn `ping -c <count> -i <interval> <target>` and parse the summary.

    Returns None on parse failure or non-zero exit with no usable output.
    The default 60 pings at 0.2s ≈ 12 seconds; the operator can opt into
    a longer 3-minute test (count=900) for flap-hunting.
    """
    try:
        result = subprocess.run(
            ["ping", "-c", str(count), "-i", str(interval_s), target],
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_s,
        )
    except (subprocess.TimeoutExpired, OSError):
        return None
    output = (result.stdout or "") + (result.stderr or "")
    return parse_ping_output(target, output)
