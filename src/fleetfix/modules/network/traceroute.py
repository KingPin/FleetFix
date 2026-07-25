"""Trace the path to a host so an operator can see *where* it stops.

Two tools, one contract. `traceroute` is the better one but is not installed on a
minimal Debian; `tracepath` ships with `iputils` and usually is. Which binary ran
— or that neither exists — is itself diagnostic information, so it lands in
`TraceResult.tool` rather than being hidden.

`trace()` never returns None (same contract as `curl_probe.probe`): failures come
back as a `TraceResult` with `.error` set, because the hops collected *before*
things went wrong are the diagnostic.
"""

from __future__ import annotations

import math
import re
import shutil
import subprocess
from dataclasses import dataclass

TRACEROUTE = "traceroute"
TRACEPATH = "tracepath"

MISSING_TOOLS_ERROR = (
    "neither traceroute nor tracepath is installed — "
    "install one with: apt install traceroute (or: apt install iputils-tracepath)"
)

# "traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets"
_HEADER_RE = re.compile(r"traceroute to \S+ \(([0-9a-fA-F.:]+)\)")
# A traceroute hop: leading number, then whatever the probes reported.
_HOP_RE = re.compile(r"^\s*(\d+)\s+(.*)$")
# A tracepath hop: "1:  192.168.1.1  0.687ms reached". The "N?:" form is an MTU
# discovery probe, not a hop, and is filtered before this matches.
_TRACEPATH_HOP_RE = re.compile(r"^\s*(\d+):\s+(.*)$")
_TRACEPATH_RTT_RE = re.compile(r"([0-9.]+)ms")
# "traceroute: unknown host x" / "tracepath: x: Name or service not known"
_TOOL_ERROR_RE = re.compile(r"^(?:traceroute|tracepath):\s*(.+)$", re.MULTILINE)

# tracepath annotates hops with these; they are not responders.
_TRACEPATH_NOISE = frozenset({"asymm", "pmtu", "reached"})
# Both tools have their own way of saying "this probe got nothing back".
_TRACEPATH_NO_ANSWER = ("no reply", "send failed")


@dataclass(frozen=True)
class TraceHop:
    number: int
    hosts: tuple[str, ...]
    rtts_ms: tuple[float, ...]
    timeouts: int
    flags: tuple[str, ...] = ()

    @property
    def responded(self) -> bool:
        return bool(self.hosts)


@dataclass(frozen=True)
class TraceResult:
    target: str
    tool: str
    hops: tuple[TraceHop, ...]
    reached: bool
    max_hops: int
    raw: str
    error: str | None = None

    @property
    def last_responding_hop(self) -> int | None:
        responders = [hop.number for hop in self.hops if hop.responded]
        return responders[-1] if responders else None

    @property
    def stalled_at(self) -> int | None:
        """First hop that never answered, when the trace never reached the target.

        None when the target was reached, or when nothing answered at all — that
        is a different diagnosis (no path off this box) and deserves its own
        wording rather than "stalled at hop 1".
        """
        if self.reached:
            return None
        last = self.last_responding_hop
        return last + 1 if last is not None else None


def parse_traceroute_output(target: str, output: str, *, max_hops: int) -> TraceResult:
    """Parse GNU/BSD traceroute output."""
    header = _HEADER_RE.search(output)
    destination = header.group(1) if header else target

    hops: list[TraceHop] = []
    for line in output.splitlines():
        match = _HOP_RE.match(line)
        if not match:
            continue
        hops.append(_parse_traceroute_hop(int(match.group(1)), match.group(2)))

    return TraceResult(
        target=target,
        tool=TRACEROUTE,
        hops=tuple(hops),
        reached=_reached_destination(hops, destination),
        max_hops=max_hops,
        raw=output,
        error=_tool_error(output) if not hops else None,
    )


def _parse_traceroute_hop(number: int, rest: str) -> TraceHop:
    """Tokenise one hop's probe results.

    Order matters within a hop: an RTT belongs to the host that most recently
    appeared, so `1.1.1.1 1.0 ms 2.2.2.2 2.0 ms` (a load-balanced hop, or `-q 3`)
    reads as two responders rather than one host with two timings.
    """
    hosts: list[str] = []
    rtts: list[float] = []
    flags: list[str] = []
    timeouts = 0

    for word in rest.split():
        if word == "*":
            timeouts += 1
        elif word == "ms":
            continue
        elif word.startswith("!"):
            flags.append(word)
        else:
            rtt = _as_float(word)
            if rtt is not None:
                rtts.append(rtt)
            else:
                hosts.append(word)

    return TraceHop(
        number=number,
        hosts=tuple(hosts),
        rtts_ms=tuple(rtts),
        timeouts=timeouts,
        flags=tuple(flags),
    )


def parse_tracepath_output(target: str, output: str, *, max_hops: int) -> TraceResult:
    """Parse iputils tracepath output.

    tracepath prints one line per *probe*, so a hop appears more than once and the
    lines have to be merged by hop number. It also has no `!H`-style flags, and
    reports completion in a trailer rather than by echoing the destination
    address, so `reached` comes from that trailer instead of the last hop's host.
    """
    merged: dict[int, TraceHop] = {}
    order: list[int] = []

    for line in output.splitlines():
        # "1?:" is an MTU discovery probe against [LOCALHOST], not a hop — the
        # trailing '?' keeps it from matching the hop pattern at all.
        match = _TRACEPATH_HOP_RE.match(line)
        if match is None:
            continue
        number = int(match.group(1))
        if number not in merged:
            merged[number] = TraceHop(number=number, hosts=(), rtts_ms=(), timeouts=0)
            order.append(number)
        merged[number] = _merge_tracepath_probe(merged[number], match.group(2))

    hops = tuple(merged[number] for number in order)
    return TraceResult(
        target=target,
        tool=TRACEPATH,
        hops=hops,
        # Both halves are needed: a maxed-out trace still prints a Resume line.
        reached="Resume:" in output and "Too many hops" not in output,
        max_hops=max_hops,
        raw=output,
        error=_tool_error(output) if not hops else None,
    )


def _merge_tracepath_probe(hop: TraceHop, rest: str) -> TraceHop:
    text = rest.strip()
    if text.startswith(_TRACEPATH_NO_ANSWER):
        return TraceHop(
            number=hop.number,
            hosts=hop.hosts,
            rtts_ms=hop.rtts_ms,
            timeouts=hop.timeouts + 1,
        )

    hosts = list(hop.hosts)
    host = text.split()[0]
    # Same responder answering a second probe is not a second responder.
    if host not in _TRACEPATH_NOISE and host not in hosts:
        hosts.append(host)

    rtts = list(hop.rtts_ms)
    rtt_match = _TRACEPATH_RTT_RE.search(text)
    if rtt_match is not None:
        value = _as_float(rtt_match.group(1))
        if value is not None:
            rtts.append(value)

    return TraceHop(
        number=hop.number,
        hosts=tuple(hosts),
        rtts_ms=tuple(rtts),
        timeouts=hop.timeouts,
    )


def select_tool() -> str:
    """Which trace binary this host has, preferring traceroute. "" when neither."""
    for tool in (TRACEROUTE, TRACEPATH):
        if shutil.which(tool) is not None:
            return tool
    return ""


def timeout_for(tool: str, *, max_hops: int = 15, wait_s: int = 1, queries: int = 1) -> float:
    """Wall-clock budget for a trace, in seconds.

    The two tools differ by nearly 3x at default settings, which is why the UI
    quotes the number: `traceroute -N 16` probes in batches, so a fully dark path
    costs one batch of waiting rather than hops*queries*wait. `tracepath` has no
    `-w` equivalent and probes serially, so its worst case scales with hop count.
    """
    if tool == TRACEROUTE:
        batches = math.ceil(max_hops * queries / 16)
        return batches * wait_s * 4 + 5
    return max_hops * 1.5 + 5


def trace(target: str, *, max_hops: int = 15, wait_s: int = 1, queries: int = 1) -> TraceResult:
    """Trace the path to `target`. Always returns a TraceResult."""
    tool = select_tool()
    if not tool:
        return TraceResult(
            target=target,
            tool="",
            hops=(),
            reached=False,
            max_hops=max_hops,
            raw="",
            error=MISSING_TOOLS_ERROR,
        )

    if tool == TRACEROUTE:
        # -n: rDNS doubles the runtime and DNS may be the broken thing.
        # -q 1: we want to know where the path stops, not per-hop jitter.
        # -w 1: the 5s default makes a black-holed path take minutes.
        # -N 16: restated so timeout_for's batch math holds on any distro build.
        argv = [
            TRACEROUTE,
            "-n",
            "-q",
            str(queries),
            "-w",
            str(wait_s),
            "-N",
            "16",
            "-m",
            str(max_hops),
            target,
        ]
    else:
        # -4 because the rest of modules/network is IPv4-only (primary_ipv4, the
        # IPv4 gateway). tracepath has no -q/-w equivalents to pass through.
        argv = [TRACEPATH, "-4", "-n", "-m", str(max_hops), target]

    timeout_s = timeout_for(tool, max_hops=max_hops, wait_s=wait_s, queries=queries)
    output, error = _run_capture(argv, timeout_s=timeout_s, tool=tool)
    if output is None:
        return TraceResult(
            target=target,
            tool=tool,
            hops=(),
            reached=False,
            max_hops=max_hops,
            raw="",
            error=error,
        )

    parser = parse_traceroute_output if tool == TRACEROUTE else parse_tracepath_output
    result = parser(target, output, max_hops=max_hops)
    if error is None:
        return result
    # A wall-clock kill still yields hops, and those hops are the answer.
    return TraceResult(
        target=result.target,
        tool=result.tool,
        hops=result.hops,
        reached=result.reached,
        max_hops=result.max_hops,
        raw=result.raw,
        error=error,
    )


def _run_capture(argv: list[str], *, timeout_s: float, tool: str) -> tuple[str | None, str | None]:
    """Run a trace and return (combined output, error).

    Popen rather than subprocess.run: on POSIX, `run(capture_output=True,
    timeout=N)` raises with `TimeoutExpired.stdout is None`, so a stalled trace
    would lose the partial hops that are the whole point. Here we kill the child
    and drain what it already wrote.
    """
    try:
        proc = subprocess.Popen(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
    except OSError as exc:
        # which() can hand back a dangling symlink, or the binary can vanish
        # between the check and the spawn.
        return None, f"{tool} unavailable: {exc}"

    try:
        output, _ = proc.communicate(timeout=timeout_s)
    except subprocess.TimeoutExpired:
        proc.kill()
        output, _ = proc.communicate()
        return output or "", f"{tool} hit the {timeout_s:g}s wall clock (partial)"
    return output or "", None


def _reached_destination(hops: list[TraceHop], destination: str) -> bool:
    if not hops:
        return False
    last = hops[-1]
    # An ICMP flag on the final hop means some *router* answered for the
    # destination (!X admin-prohibited, !H host-unreachable) — not the host.
    return destination in last.hosts and not last.flags


def _tool_error(output: str) -> str | None:
    match = _TOOL_ERROR_RE.search(output)
    return match.group(1).strip() if match else None


def _as_float(token: str) -> float | None:
    try:
        return float(token)
    except ValueError:
        return None
