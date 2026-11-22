from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any, Mapping

from .errors import ErrorCode, HapticError, Issue
from .models import PacketState

ALLOWED_TRANSITIONS: Mapping[PacketState, frozenset[PacketState]] = {
    PacketState.PENDING: frozenset({PacketState.ENCODED, PacketState.FAILED}),
    PacketState.ENCODED: frozenset({PacketState.DISPATCHED, PacketState.FAILED}),
    PacketState.DISPATCHED: frozenset({PacketState.CONFIRMED, PacketState.FAILED}),
    PacketState.CONFIRMED: frozenset(),
    PacketState.FAILED: frozenset(
        {PacketState.ENCODED, PacketState.DISPATCHED, PacketState.CONFIRMED}
    ),
}


@dataclass(frozen=True)
class StateTransition:
    packet_seq: int
    channel: int
    old_state: PacketState
    new_state: PacketState
    reason: str


class PacketStateMachine:
    """Thread-safe packet lifecycle state machine.

    Invalid transitions raise E_STATE and leave the original state untouched.
    """

    def __init__(self) -> None:
        self._states: dict[int, PacketState] = {}
        self._channels: dict[int, int] = {}
        self._history: list[StateTransition] = []
        self._lock = threading.RLock()

    def register(self, packet_seq: int, channel: int, state: PacketState = PacketState.PENDING) -> None:
        with self._lock:
            if packet_seq in self._states:
                raise HapticError(
                    Issue(
                        ErrorCode.E_STATE,
                        "packet is already registered",
                        packet_seq=packet_seq,
                        channel=channel,
                        field="packet_seq",
                    )
                )
            self._states[packet_seq] = state
            self._channels[packet_seq] = channel

    def state(self, packet_seq: int) -> PacketState:
        with self._lock:
            return self._states[packet_seq]

    def transition(self, packet_seq: int, new_state: PacketState, reason: str = "") -> StateTransition:
        with self._lock:
            old_state = self._states.get(packet_seq)
            if old_state is None:
                raise HapticError(
                    Issue(
                        ErrorCode.E_STATE,
                        "packet is not registered",
                        packet_seq=packet_seq,
                        field="packet_seq",
                    )
                )
            if new_state not in ALLOWED_TRANSITIONS[old_state]:
                raise HapticError(
                    Issue(
                        ErrorCode.E_STATE,
                        "illegal packet state transition",
                        packet_seq=packet_seq,
                        channel=self._channels[packet_seq],
                        field="state",
                        details={
                            "old_state": old_state.value,
                            "requested_state": new_state.value,
                            "allowed": sorted(
                                item.value for item in ALLOWED_TRANSITIONS[old_state]
                            ),
                        },
                    )
                )
            self._states[packet_seq] = new_state
            transition = StateTransition(
                packet_seq=packet_seq,
                channel=self._channels[packet_seq],
                old_state=old_state,
                new_state=new_state,
                reason=reason,
            )
            self._history.append(transition)
            return transition

    def history(self) -> tuple[StateTransition, ...]:
        with self._lock:
            return tuple(self._history)

    def snapshot(self) -> dict[int, PacketState]:
        with self._lock:
            return dict(self._states)

    def to_dict(self) -> dict[str, Any]:
        with self._lock:
            return {
                "states": {
                    str(seq): state.value for seq, state in sorted(self._states.items())
                },
                "history": [
                    {
                        "packet_seq": item.packet_seq,
                        "channel": item.channel,
                        "old_state": item.old_state.value,
                        "new_state": item.new_state.value,
                        "reason": item.reason,
                    }
                    for item in self._history
                ],
            }

