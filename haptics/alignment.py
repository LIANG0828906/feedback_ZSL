from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Any, Iterable, Mapping

from .errors import ErrorCode, Issue
from .models import Echo, EncodedPacket


@dataclass(frozen=True)
class PacketAlignment:
    packet_seq: int
    channel: int
    local_index: int
    sent_at_ms: float
    echoes: tuple[Echo, ...]
    issue: Issue | None
    retry_count: int = 0

    @property
    def status(self) -> str:
        return "OK" if self.issue is None else self.issue.code.value

    @property
    def retryable(self) -> bool:
        return self.issue is not None and self.issue.code != ErrorCode.E_LOCAL

    def first_seen_ms(self) -> float | None:
        if self.issue is None:
            return self.echoes[0].processed_at_ms if self.echoes else None
        return self.issue.first_seen_ms

    def to_dict(self) -> dict[str, Any]:
        return {
            "packet_seq": self.packet_seq,
            "channel": self.channel,
            "local_index": self.local_index,
            "sent_at_ms": self.sent_at_ms,
            "status": self.status,
            "issue": None if self.issue is None else self.issue.to_dict(),
            "retry_count": self.retry_count,
            "echoes": [echo.to_dict() for echo in self.echoes],
        }


@dataclass(frozen=True)
class AlignmentReport:
    packets: tuple[PacketAlignment, ...]
    issues: tuple[Issue, ...]
    local_order_ok: bool
    timeout_ms: float
    stats: Mapping[str, int]

    @classmethod
    def build(
        cls,
        local_packets: Iterable[EncodedPacket],
        echoes: Iterable[Echo],
        timeout_ms: float,
    ) -> "AlignmentReport":
        packets = tuple(local_packets)
        echo_list = tuple(echoes)

        local_order_issues: list[Issue] = []
        previous_seq = -1
        for packet in packets:
            if packet.packet_seq <= previous_seq:
                matching_echo = next(
                    (
                        echo
                        for echo in echo_list
                        if echo.received_seq == packet.packet_seq
                    ),
                    None,
                )
                local_order_issues.append(
                    Issue(
                        ErrorCode.E_LOCAL,
                        "local packet sequence is inverted or duplicated",
                        packet_seq=packet.packet_seq,
                        channel=packet.channel,
                        field="packet_seq",
                        first_seen_ms=(
                            matching_echo.processed_at_ms
                            if matching_echo is not None
                            else float(packet.sent_at_ms or 0.0)
                        ),
                        details={"previous_packet_seq": previous_seq},
                    )
                )
            previous_seq = packet.packet_seq
        if local_order_issues:
            alignments: list[PacketAlignment] = []
            for packet in packets:
                matching_issue = next(
                    (issue for issue in local_order_issues if issue.packet_seq == packet.packet_seq),
                    None,
                )
                alignments.append(
                    PacketAlignment(
                        packet_seq=packet.packet_seq,
                        channel=packet.channel,
                        local_index=packet.local_index,
                        sent_at_ms=float(packet.sent_at_ms or 0.0),
                        echoes=tuple(
                            echo for echo in echo_list if echo.received_seq == packet.packet_seq
                        ),
                        issue=matching_issue,
                    )
                )
            return cls._from_alignments(
                tuple(alignments),
                local_order_ok=False,
                timeout_ms=timeout_ms,
            )

        by_seq: dict[int, list[Echo]] = {}
        for echo in echo_list:
            by_seq.setdefault(echo.received_seq, []).append(echo)

        deliveries = sorted(echo_list, key=lambda echo: echo.delivered_index)
        reorder_sequences: set[int] = set()
        for left, right in zip(deliveries, deliveries[1:]):
            if left.received_seq > right.received_seq:
                reorder_sequences.add(left.received_seq)
                reorder_sequences.add(right.received_seq)

        result: list[PacketAlignment] = []
        for packet in packets:
            sent_at = float(packet.sent_at_ms or 0.0)
            matching = tuple(sorted(by_seq.get(packet.packet_seq, ()), key=lambda e: e.attempt))
            issue: Issue | None = None
            if not matching:
                issue = Issue(
                    ErrorCode.E_MISS,
                    "device echo is missing",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field="received_seq",
                    first_seen_ms=sent_at,
                )
            elif len(matching) > 1:
                issue = Issue(
                    ErrorCode.E_DUP,
                    "device echo was delivered more than once",
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    field="received_seq",
                    first_seen_ms=min(echo.processed_at_ms for echo in matching),
                    details={"echo_count": len(matching)},
                )
            else:
                echo = matching[0]
                if echo.processed_at_ms - sent_at > timeout_ms + 1e-9:
                    issue = Issue(
                        ErrorCode.E_TIMEOUT,
                        "device echo arrived after the dispatch timeout",
                        packet_seq=packet.packet_seq,
                        channel=packet.channel,
                        field="processed_at_ms",
                        first_seen_ms=echo.processed_at_ms,
                        details={
                            "sent_at_ms": sent_at,
                            "timeout_ms": timeout_ms,
                            "latency_ms": round(echo.processed_at_ms - sent_at, 3),
                        },
                    )
                elif packet.packet_seq in reorder_sequences:
                    issue = Issue(
                        ErrorCode.E_REORDER,
                        "service delivery order differs from local packet order",
                        packet_seq=packet.packet_seq,
                        channel=packet.channel,
                        field="delivered_index",
                        first_seen_ms=echo.processed_at_ms,
                        details={"delivered_index": echo.delivered_index},
                    )
            result.append(
                PacketAlignment(
                    packet_seq=packet.packet_seq,
                    channel=packet.channel,
                    local_index=packet.local_index,
                    sent_at_ms=sent_at,
                    echoes=matching,
                    issue=issue,
                )
            )

        known = {packet.packet_seq for packet in packets}
        for echo in echo_list:
            if echo.received_seq not in known:
                result.append(
                    PacketAlignment(
                        packet_seq=echo.received_seq,
                        channel=echo.channel,
                        local_index=-1,
                        sent_at_ms=0.0,
                        echoes=(echo,),
                        issue=Issue(
                            ErrorCode.E_LOCAL,
                            "device echoed a packet sequence absent from local records",
                            packet_seq=echo.received_seq,
                            channel=echo.channel,
                            field="received_seq",
                            first_seen_ms=echo.processed_at_ms,
                        ),
                    )
                )
        return cls._from_alignments(tuple(result), local_order_ok=True, timeout_ms=timeout_ms)

    @classmethod
    def _from_alignments(
        cls,
        alignments: tuple[PacketAlignment, ...],
        *,
        local_order_ok: bool,
        timeout_ms: float,
    ) -> "AlignmentReport":
        issues = tuple(item.issue for item in alignments if item.issue is not None)
        stats: dict[str, int] = {"OK": 0}
        for code in (
            ErrorCode.E_MISS,
            ErrorCode.E_DUP,
            ErrorCode.E_TIMEOUT,
            ErrorCode.E_REORDER,
            ErrorCode.E_LOCAL,
        ):
            stats[code.value] = 0
        for issue in issues:
            stats[issue.code.value] = stats.get(issue.code.value, 0) + 1
        stats["OK"] = len(alignments) - len(issues)
        stats["TOTAL"] = len(alignments)
        stats["RETRYABLE"] = sum(
            1 for item in alignments if item.issue is not None and item.retryable
        )
        return cls(
            packets=alignments,
            issues=issues,
            local_order_ok=local_order_ok,
            timeout_ms=timeout_ms,
            stats=stats,
        )

    def packet(self, packet_seq: int) -> PacketAlignment | None:
        return next((item for item in self.packets if item.packet_seq == packet_seq), None)

    def with_retry(self, packet_seq: int, echo: Echo) -> "AlignmentReport":
        """Apply one retry result without changing any other packet observation."""

        updated: list[PacketAlignment] = []
        found = False
        for item in self.packets:
            if item.packet_seq != packet_seq:
                updated.append(item)
                continue
            found = True
            latency = echo.processed_at_ms - item.sent_at_ms
            issue: Issue | None = None
            if latency > self.timeout_ms + 1e-9:
                issue = Issue(
                    ErrorCode.E_TIMEOUT,
                    "retry echo still arrived after the dispatch timeout",
                    packet_seq=packet_seq,
                    channel=item.channel,
                    field="processed_at_ms",
                    first_seen_ms=echo.processed_at_ms,
                    details={"latency_ms": round(latency, 3), "timeout_ms": self.timeout_ms},
                )
            updated.append(
                replace(
                    item,
                    echoes=(echo,),
                    issue=issue,
                    retry_count=item.retry_count + 1,
                )
            )
        if not found:
            raise KeyError(f"packet {packet_seq} is not present in alignment report")
        return self._from_alignments(
            tuple(updated),
            local_order_ok=self.local_order_ok,
            timeout_ms=self.timeout_ms,
        )

    def deterministic_dict(self) -> dict[str, Any]:
        return {
            "local_order_ok": self.local_order_ok,
            "timeout_ms": self.timeout_ms,
            "stats": dict(sorted(self.stats.items())),
            "packets": [
                {
                    "packet_seq": item.packet_seq,
                    "channel": item.channel,
                    "status": item.status,
                    "retry_count": item.retry_count,
                    "echoes": [
                        {
                            "received_seq": echo.received_seq,
                            "channel": echo.channel,
                            "service_rx_index": echo.service_rx_index,
                            "delivered_index": echo.delivered_index,
                            "processed_at_ms": echo.processed_at_ms,
                            "attempt": echo.attempt,
                        }
                        for echo in item.echoes
                    ],
                    "issue": None if item.issue is None else item.issue.to_dict(),
                }
                for item in sorted(self.packets, key=lambda item: item.packet_seq)
            ],
        }

