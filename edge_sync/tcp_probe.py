"""Connection-level congestion signal collection.

Two platform backends are provided without modifying the OS protocol stack:

* Linux: ``TCP_INFO`` socket option (``struct tcp_info``).
* Windows: ``SIO_TCP_INFO`` control code (``TCP_INFO_v0``) via ``ctypes``.

Every value is either a real kernel readout or explicitly reported as
missing with a stable business error code; estimates are always labelled as
approximations and never presented as raw kernel readings.
"""

from __future__ import annotations

import platform
import sys
from dataclasses import dataclass, field

from . import ERR_SIGNAL_COLLECTION, ERR_SIGNAL_UNSUPPORTED


@dataclass
class TcpSignals:
    platform: str
    rtt_ms: float | None = None
    rtt_var_ms: float | None = None
    rto_ms: int | None = None
    rto_count: int | None = None
    retrans_segments: int | None = None
    cwnd_segments: int | None = None
    mss: int | None = None
    bytes_in_flight: int | None = None
    missing: list[str] = field(default_factory=list)
    missing_codes: list[str] = field(default_factory=list)
    note: str = ""

    def delta(self, earlier: "TcpSignals") -> dict:
        """Return per-upload deltas for cumulative counters."""

        def diff(name: str) -> int | None:
            current = getattr(self, name)
            base = getattr(earlier, name)
            if current is None or base is None:
                return None
            return max(0, current - base)

        return {
            "rtt_ms": self.rtt_ms,
            "rtt_var_ms": self.rtt_var_ms,
            "rto_ms": self.rto_ms,
            "rto_count_delta": diff("rto_count"),
            "retrans_delta": diff("retrans_segments"),
            "cwnd_segments": self.cwnd_segments,
            "mss": self.mss,
            "bytes_in_flight": self.bytes_in_flight,
            "missing": sorted(set(self.missing) | set(earlier.missing)),
            "missing_codes": sorted(set(self.missing_codes) | set(earlier.missing_codes)),
            "platform": self.platform,
            "note": self.note,
        }


# Prefix of struct tcp_info, twenty tightly packed little-endian u32 fields.
# Verified against a 6.18 kernel byte layout:
#   rto_us[2], ato_us[3], snd_mss[4], total_retrans[10], rtt_us[13],
#   rttvar_us[14], snd_cwnd[17].
# The classic RTO-timeout counter (tcpi_retransmits) moved after the u32
# prefix in this kernel; it is therefore reported as unavailable instead of
# misreading the RTO microsecond value.
_LINUX_TCP_INFO_FMT = "<20I"
_IDX_RTO_US = 2
_IDX_RTT_US = 13
_IDX_RTTVAR_US = 14
_IDX_TOTAL_RETRANS = 10
_IDX_SND_CWND = 17
_IDX_SND_MSS = 4


def _probe_linux(sock) -> TcpSignals:
    import socket as socket_mod
    import struct

    signals = TcpSignals(platform="linux")
    try:
        raw = sock.getsockopt(socket_mod.IPPROTO_TCP, socket_mod.TCP_INFO, 256)
    except OSError as exc:
        signals.missing = [
            "rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments",
        ]
        signals.missing_codes = [ERR_SIGNAL_COLLECTION]
        signals.note = f"TCP_INFO unavailable: {exc}"
        return signals
    expected = struct.calcsize(_LINUX_TCP_INFO_FMT)
    if len(raw) < expected:
        signals.missing = [
            "rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments",
        ]
        signals.missing_codes = [ERR_SIGNAL_COLLECTION]
        signals.note = f"TCP_INFO payload {len(raw)}B smaller than expected {expected}B"
        return signals
    fields = struct.unpack(_LINUX_TCP_INFO_FMT, raw[:expected])
    signals.retrans_segments = int(fields[_IDX_TOTAL_RETRANS])
    signals.rto_ms = max(0, round(int(fields[_IDX_RTO_US]) / 1000.0))
    rtt_us = int(fields[_IDX_RTT_US])
    rtt_var_us = int(fields[_IDX_RTTVAR_US])
    signals.cwnd_segments = int(fields[_IDX_SND_CWND])
    signals.mss = int(fields[_IDX_SND_MSS]) or None
    signals.rto_count = None
    signals.missing.append("rto_count")
    signals.missing_codes.append(ERR_SIGNAL_UNSUPPORTED)
    signals.rtt_ms = round(rtt_us / 1000.0, 4) if rtt_us else None
    signals.rtt_var_ms = round(rtt_var_us / 1000.0, 4) if rtt_var_us else None
    if signals.rtt_ms is None:
        signals.missing.append("rtt_ms")
        signals.missing_codes.append(ERR_SIGNAL_COLLECTION)
    signals.note = "kernel TCP_INFO"
    return signals


# SIO_TCP_INFO control code (documented in mstcpip.h / Windows SDK).
_SIO_TCP_INFO = 0x18000077
# TCP_INFO_v0 (mstcpip.h), 96 bytes on x86/x64 Windows:
# 11 u32, then 3 u64, then 6 u32, then 4 bytes trailing padding.
_WIN_TCP_INFO_V0 = "<11I3Q6I4x"


def _probe_windows(sock) -> TcpSignals:
    import ctypes
    from ctypes import wintypes

    signals = TcpSignals(platform="windows")
    ws2_32 = ctypes.WinDLL("ws2_32")  # type: ignore[attr-defined]
    version = wintypes.DWORD(0)
    size = ctypes.c_int(96)
    out = ctypes.create_string_buffer(96)
    returned = wintypes.DWORD(0)
    handle = ctypes.c_void_p(int(sock.fileno()))
    result = ws2_32.WSAIoctl(
        handle,
        wintypes.DWORD(_SIO_TCP_INFO),
        ctypes.byref(version),
        ctypes.sizeof(version),
        out,
        size,
        ctypes.byref(returned),
        None,
        None,
    )
    if result != 0:
        signals.missing = [
            "rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments",
        ]
        signals.missing_codes = [ERR_SIGNAL_COLLECTION]
        signals.note = f"WSAIoctl(SIO_TCP_INFO) failed: {ws2_32.WSAGetLastError()}"
        return signals
    import struct

    values = struct.unpack(_WIN_TCP_INFO_V0, out.raw[:96])
    # Indices follow the corrected layout: RttUs(3), RttVarUs(4),
    # ReXmitCount(9), RtoUs(5), DwMss(1), BytesInFlight(17).
    rtt_us, rtt_var_us = values[3], values[4]
    signals.retrans_segments = int(values[9])
    signals.rto_ms = max(0, round(int(values[5]) / 1000.0))
    signals.mss = int(values[1]) or None
    signals.bytes_in_flight = int(values[17])
    signals.rtt_ms = round(rtt_us / 1000.0, 4) if rtt_us else None
    signals.rtt_var_ms = round(rtt_var_us / 1000.0, 4) if rtt_var_us else None
    if signals.mss and signals.bytes_in_flight is not None:
        # Explicitly labelled approximation derived from a real readout;
        # Windows TCP_INFO_v0 exposes no direct congestion window field.
        signals.cwnd_segments = max(1, (signals.bytes_in_flight + signals.mss - 1) // signals.mss)
        signals.note = "SIO_TCP_INFO; cwnd is os-derived approximation from BytesInFlight/MSS"
    else:
        signals.missing.append("cwnd_segments")
        signals.missing_codes.append(ERR_SIGNAL_COLLECTION)
        signals.note = "SIO_TCP_INFO"
    # TCP_INFO_v0 has no RTO-timeout counter.
    signals.rto_count = None
    signals.missing.append("rto_count")
    signals.missing_codes.append(ERR_SIGNAL_UNSUPPORTED)
    return signals


def probe(sock) -> TcpSignals:
    """Collect a congestion-signal snapshot for a connected socket."""
    try:
        if sys.platform.startswith("linux"):
            return _probe_linux(sock)
        if sys.platform == "win32":
            return _probe_windows(sock)
    except Exception as exc:  # pragma: no cover - defensive boundary
        signals = TcpSignals(platform=platform.system().lower())
        signals.missing = [
            "rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments",
        ]
        signals.missing_codes = [ERR_SIGNAL_COLLECTION]
        signals.note = f"probe exception: {type(exc).__name__}: {exc}"
        return signals
    signals = TcpSignals(platform=platform.system().lower())
    signals.missing = [
        "rtt_ms", "rto_ms", "rto_count", "retrans_segments", "cwnd_segments",
    ]
    signals.missing_codes = [ERR_SIGNAL_UNSUPPORTED]
    signals.note = f"unsupported platform {sys.platform}"
    return signals
