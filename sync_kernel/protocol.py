from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import (
    E_DECODE,
    E_SEQ,
    E_SEMANTIC,
    E_VER,
    KernelError,
    KernelIssue,
    warn_lossy,
)
from .models import (
    LOGIC_HZ,
    AuthoritativeState,
    MatchContext,
    UnitState,
)
from .util import canonical_json_bytes, is_int, parse_json_payload


SUPPORTED_VERSIONS = (1, 2, 3)


PROTOCOL_SPECS: dict[int, dict[str, Any]] = {
    1: {
        "protocol_version": 1,
        "name": "legacy-tick-delta",
        "transport_fields": {
            "header": {
                "protocol_version": "required integer",
                "type": "required literal 'sync'",
                "sequence": "required monotonic frame sequence",
                "server_tick": "required server tick count",
                "full": "required boolean; full only at sequence 0 or explicit resync",
                "match": "required match identifier",
                "units": "required array of changed unit objects",
            },
            "unit": {
                "id": "required unit identifier",
                "p": "[x_milliunit, y_milliunit]",
                "rot": "heading in millidegrees",
                "hp": "current health in milli-HP",
                "hp_max": "maximum health in milli-HP",
                "cd": "remaining cooldown in ticks",
                "mana": "resource in milli-units",
                "st": "status bit mask",
                "owner": "owner player identifier",
                "team": "team identifier",
            },
        },
        "time_base": {
            "field": "server_tick",
            "unit": "tick",
            "conversion": "internal_tick = server_tick",
        },
        "delta_encoding": "changed_fields_only",
        "missing_field_semantics": "retain_previous_value",
        "full_snapshot_support": "only tick 0 or explicit full=true",
        "lossy_fields": ["logical_clock", "snapshot_hash", "source_time_base"],
    },
    2: {
        "protocol_version": 2,
        "name": "microsecond-delta-with-mask-semantics",
        "transport_fields": {
            "header": {
                "protocol_version": "required integer",
                "kind": "required literal 'sync_delta'",
                "sequence": "required monotonic frame sequence",
                "server_time_us": "required server time in microseconds",
                "full_snapshot": "required boolean",
                "match_id": "required match identifier",
                "content_hash8": "required first 8 hex characters of state hash",
                "units": "required array of changed entities",
            },
            "unit": {
                "unit_id": "required unit identifier",
                "owner_id": "owner player identifier",
                "team": "team identifier",
                "position": "{x, y} in milli-units",
                "facing_mdeg": "heading in millidegrees",
                "health": "{current, maximum} in milli-HP",
                "cooldown_ticks": "remaining cooldown in ticks",
                "resource_units": "resource in milli-units",
                "status": "status mask; omitted means explicit zero",
            },
        },
        "time_base": {
            "field": "server_time_us",
            "unit": "microsecond",
            "conversion": "internal_tick = round(server_time_us * tick_rate / 1_000_000)",
        },
        "delta_encoding": "changed_field_groups",
        "missing_field_semantics": "non_status_fields_retain_previous_status_omitted_means_zero",
        "full_snapshot_support": "tick 0 or explicit full_snapshot=true",
        "lossy_fields": ["logical_clock", "snapshot_hash", "source_time_base"],
    },
    3: {
        "protocol_version": 3,
        "name": "logical-clock-snapshot-delta",
        "transport_fields": {
            "header": {
                "protocol_version": "required integer",
                "kind": "required literal 'snapshot_delta'",
                "sequence": "required monotonic frame sequence",
                "logical_clock": "required monotonic logical clock at 120 Hz",
                "match_id": "required match identifier",
                "snapshot_hash": "required SHA-256 over the authoritative state payload",
                "snapshot": "required complete unit snapshot",
                "delta": "required changed-field projection relative to previous frame",
            },
            "snapshot_unit": {
                "owner_id": "owner player identifier",
                "team": "team identifier",
                "position": "{x, y} in milli-units",
                "heading_mdeg": "heading in millidegrees",
                "health_current": "current milli-HP",
                "health_maximum": "maximum milli-HP",
                "cooldown_ticks": "remaining ticks",
                "resource_units": "resource in milli-units",
                "status_mask": "status bit mask",
            },
        },
        "time_base": {
            "field": "logical_clock",
            "unit": "logical tick at 120 Hz",
            "conversion": "internal_tick = logical_clock // 120",
        },
        "delta_encoding": "full_snapshot_plus_changed_field_delta",
        "missing_field_semantics": "snapshot_is_complete_delta_omission_retains_snapshot",
        "full_snapshot_support": "every frame",
        "lossy_fields": [],
    },
}


@dataclass
class DecodedFrame:
    state: AuthoritativeState
    source_version: int
    source_sequence: int
    source_time: int
    time_base: str
    was_full: bool
    frame_bytes: bytes = b""
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass
class EncodeResult:
    frame: dict[str, Any]
    payload: bytes
    target_version: int
    warnings: list[KernelIssue] = field(default_factory=list)
    mappings: list[dict[str, Any]] = field(default_factory=list)

    @property
    def warning_codes(self) -> list[str]:
        return [warning.code for warning in self.warnings]


def time_us_for_tick(tick: int, tick_rate: int = LOGIC_HZ) -> int:
    return (tick * 1_000_000 + tick_rate - 1) // tick_rate


def tick_from_time_us(time_us: int, tick_rate: int = LOGIC_HZ) -> int:
    return (time_us * tick_rate + 500_000) // 1_000_000


def _required(frame: dict[str, Any], key: str, version: int) -> Any:
    if key not in frame:
        raise KernelError(
            E_VER,
            f"required field {key!r} is missing for protocol version {version}",
            path=f"$.{key}",
            source_version=version,
        )
    return frame[key]


def _required_int(
    frame: dict[str, Any],
    key: str,
    version: int,
    *,
    minimum: int | None = None,
) -> int:
    value = _required(frame, key, version)
    if not is_int(value):
        raise KernelError(
            E_DECODE,
            f"field {key!r} must be an integer",
            path=f"$.{key}",
            source_version=version,
        )
    if minimum is not None and value < minimum:
        raise KernelError(
            E_SEMANTIC,
            f"field {key!r} must be >= {minimum}",
            path=f"$.{key}",
            source_version=version,
            details={"value": value, "minimum": minimum},
        )
    return value


def _required_bool(frame: dict[str, Any], key: str, version: int) -> bool:
    value = _required(frame, key, version)
    if not isinstance(value, bool):
        raise KernelError(
            E_DECODE,
            f"field {key!r} must be a boolean",
            path=f"$.{key}",
            source_version=version,
        )
    return value


def _required_str(frame: dict[str, Any], key: str, version: int, path: str | None = None) -> str:
    value = _required(frame, key, version)
    if not isinstance(value, str) or not value:
        raise KernelError(
            E_DECODE,
            f"field {key!r} must be a non-empty string",
            path=path or f"$.{key}",
            source_version=version,
        )
    return value


def _field_int(
    container: dict[str, Any],
    key: str,
    path: str,
    version: int,
    *,
    required: bool,
) -> int | None:
    if key not in container:
        if required:
            raise KernelError(
                E_VER,
                f"required field {key!r} is missing for protocol version {version}",
                path=f"{path}.{key}",
                source_version=version,
            )
        return None
    value = container[key]
    if not is_int(value):
        raise KernelError(
            E_DECODE,
            f"field {key!r} must be an integer",
            path=f"{path}.{key}",
            source_version=version,
        )
    return value


def _field_str(
    container: dict[str, Any],
    key: str,
    path: str,
    version: int,
    *,
    required: bool,
) -> str | None:
    if key not in container:
        if required:
            raise KernelError(
                E_VER,
                f"required field {key!r} is missing for protocol version {version}",
                path=f"{path}.{key}",
                source_version=version,
            )
        return None
    value = container[key]
    if not isinstance(value, str) or not value:
        raise KernelError(
            E_DECODE,
            f"field {key!r} must be a non-empty string",
            path=f"{path}.{key}",
            source_version=version,
        )
    return value


def _position(
    value: Any,
    path: str,
    version: int,
) -> tuple[int, int]:
    if not isinstance(value, (list, dict)):
        raise KernelError(
            E_DECODE,
            "position must be [x,y] or {x,y}",
            path=path,
            source_version=version,
        )
    if isinstance(value, list):
        if len(value) != 2 or not all(is_int(item) for item in value):
            raise KernelError(
                E_DECODE,
                "position array must contain exactly two integers",
                path=path,
                source_version=version,
            )
        return int(value[0]), int(value[1])
    x = value.get("x")
    y = value.get("y")
    if not is_int(x) or not is_int(y):
        raise KernelError(
            E_DECODE,
            "position object requires integer x and y",
            path=path,
            source_version=version,
        )
    return x, y


def _health(value: Any, path: str, version: int) -> tuple[int, int]:
    if not isinstance(value, dict):
        raise KernelError(
            E_DECODE,
            "health must be an object",
            path=path,
            source_version=version,
        )
    current = value.get("current")
    maximum = value.get("maximum")
    if not is_int(current) or not is_int(maximum):
        raise KernelError(
            E_DECODE,
            "health requires integer current and maximum",
            path=path,
            source_version=version,
        )
    return current, maximum


def _clone_base_state(
    base: AuthoritativeState | None,
    context: MatchContext | None,
    frame_match_id: str,
    version: int,
) -> AuthoritativeState:
    if base is not None:
        if frame_match_id != base.match_id:
            raise KernelError(
                E_SEMANTIC,
                "frame match_id does not match the base state",
                path="$.match_id",
                source_version=version,
                details={"frame_match_id": frame_match_id, "base_match_id": base.match_id},
            )
        state = base.clone()
        if context is not None and context.seed != state.seed:
            raise KernelError(
                E_SEMANTIC,
                "frame context seed does not match the base state",
                path="$.seed",
                source_version=version,
                details={"context_seed": context.seed, "base_seed": state.seed},
            )
        return state
    if context is None:
        raise KernelError(
            E_SEMANTIC,
            "decoding a full frame requires MatchContext or a base state",
            path="$",
            source_version=version,
        )
    if frame_match_id != context.match_id:
        raise KernelError(
            E_SEMANTIC,
            "frame match_id does not match MatchContext",
            path="$.match_id",
            source_version=version,
            details={"frame_match_id": frame_match_id, "context_match_id": context.match_id},
        )
    return AuthoritativeState(
        match_id=context.match_id,
        seed=context.seed,
        tick=0,
        logical_clock=0,
        sequence=0,
        units={},
        source_version=version,
    )


def _check_delta_tick(base: AuthoritativeState | None, tick: int, path: str, version: int) -> None:
    if base is not None and tick <= base.tick:
        raise KernelError(
            E_SEMANTIC,
            "frame time did not advance monotonically",
            path=path,
            source_version=version,
            details={"base_tick": base.tick, "frame_tick": tick},
        )
    if base is not None and tick != base.tick + 1:
        raise KernelError(
            E_SEQ,
            "delta frame is not adjacent to the supplied base state",
            path=path,
            source_version=version,
            details={"expected_tick": base.tick + 1, "frame_tick": tick},
        )


def _finish_decoded_state(
    state: AuthoritativeState,
    tick: int,
    sequence: int,
    logical_clock: int,
    version: int,
    time_base: str,
) -> AuthoritativeState:
    state.tick = tick
    state.sequence = sequence
    state.logical_clock = logical_clock
    state.input_offset = tick
    state.rng_progress = tick * len(state.units)
    state.source_version = version
    state.source_time_base = time_base
    state.validate()
    state.rehash()
    return state


def _decode_v1(
    frame: dict[str, Any],
    base: AuthoritativeState | None,
    context: MatchContext | None,
) -> DecodedFrame:
    version = 1
    frame_type = _required(frame, "type", version)
    if frame_type != "sync":
        raise KernelError(
            E_DECODE,
            "v1 frame type must be 'sync'",
            path="$.type",
            source_version=version,
        )
    sequence = _required_int(frame, "sequence", version, minimum=0)
    tick = _required_int(frame, "server_tick", version, minimum=0)
    full = _required_bool(frame, "full", version)
    match_id = _required_str(frame, "match", version)
    _required(frame, "units", version)
    units_data = frame["units"]
    if not isinstance(units_data, list):
        raise KernelError(
            E_DECODE,
            "v1 units must be an array",
            path="$.units",
            source_version=version,
        )
    if not full:
        if base is None:
            raise KernelError(
                E_SEMANTIC,
                "v1 delta frame requires a base state",
                path="$.full",
                source_version=version,
            )
        _check_delta_tick(base, tick, "$.server_tick", version)
    elif base is not None and tick < base.tick:
        raise KernelError(
            E_SEMANTIC,
            "v1 server tick moved backwards",
            path="$.server_tick",
            source_version=version,
            details={"base_tick": base.tick, "frame_tick": tick},
        )
    state = _clone_base_state(base, context, match_id, version)
    if full:
        state.units = {}

    required_full = ("id", "p", "rot", "hp", "hp_max", "cd", "mana", "st", "owner", "team")
    for index, entry in enumerate(units_data):
        path = f"$.units[{index}]"
        if not isinstance(entry, dict):
            raise KernelError(
                E_DECODE,
                "v1 unit entry must be an object",
                path=path,
                source_version=version,
            )
        unit_id = _field_str(entry, "id", path, version, required=True)
        assert unit_id is not None
        if full:
            for key in required_full:
                if key not in entry:
                    raise KernelError(
                        E_VER,
                        f"required full-snapshot field {key!r} is missing",
                        path=f"{path}.{key}",
                        source_version=version,
                        object_id=unit_id,
                    )
            owner_id = _field_str(entry, "owner", path, version, required=True)
            team = _field_str(entry, "team", path, version, required=True)
            assert owner_id is not None and team is not None
            existing = UnitState(
                unit_id=unit_id,
                owner_id=owner_id,
                team=team,
                x=0,
                y=0,
                heading=0,
                hp=1,
                max_hp=1,
            )
        else:
            if unit_id not in state.units:
                raise KernelError(
                    E_SEMANTIC,
                    "v1 delta references an unknown unit",
                    path=f"{path}.id",
                    source_version=version,
                    object_id=unit_id,
                )
            existing = state.units[unit_id].clone()

        if "p" in entry:
            existing.x, existing.y = _position(entry["p"], f"{path}.p", version)
        if "rot" in entry:
            value = _field_int(entry, "rot", path, version, required=full)
            if value is not None:
                existing.heading = value
        if "hp" in entry:
            value = _field_int(entry, "hp", path, version, required=full)
            if value is not None:
                existing.hp = value
        if "hp_max" in entry:
            value = _field_int(entry, "hp_max", path, version, required=full)
            if value is not None:
                existing.max_hp = value
        if "cd" in entry:
            value = _field_int(entry, "cd", path, version, required=full)
            if value is not None:
                existing.cooldown = value
        if "mana" in entry:
            value = _field_int(entry, "mana", path, version, required=full)
            if value is not None:
                existing.resource = value
        if "st" in entry:
            value = _field_int(entry, "st", path, version, required=full)
            if value is not None:
                existing.status_mask = value
        if "owner" in entry:
            value = _field_str(entry, "owner", path, version, required=full)
            if value is not None:
                existing.owner_id = value
        if "team" in entry:
            value = _field_str(entry, "team", path, version, required=full)
            if value is not None:
                existing.team = value
        state.units[unit_id] = existing

    if not state.units:
        raise KernelError(
            E_SEMANTIC,
            "decoded state has no units",
            path="$.units",
            source_version=version,
        )
    state = _finish_decoded_state(
        state,
        tick=tick,
        sequence=sequence,
        logical_clock=tick * (context.clock_hz if context else LOGIC_HZ),
        version=version,
        time_base="server_tick",
    )
    return DecodedFrame(
        state=state,
        source_version=version,
        source_sequence=sequence,
        source_time=tick,
        time_base="server_tick",
        was_full=full,
    )


def _decode_v2(
    frame: dict[str, Any],
    base: AuthoritativeState | None,
    context: MatchContext | None,
) -> DecodedFrame:
    version = 2
    kind = _required(frame, "kind", version)
    if kind != "sync_delta":
        raise KernelError(
            E_DECODE,
            "v2 frame kind must be 'sync_delta'",
            path="$.kind",
            source_version=version,
        )
    sequence = _required_int(frame, "sequence", version, minimum=0)
    server_time_us = _required_int(frame, "server_time_us", version, minimum=0)
    full = _required_bool(frame, "full_snapshot", version)
    match_id = _required_str(frame, "match_id", version)
    content_hash8 = _required_str(frame, "content_hash8", version)
    if len(content_hash8) != 8:
        raise KernelError(
            E_DECODE,
            "v2 content_hash8 must contain exactly 8 hex characters",
            path="$.content_hash8",
            source_version=version,
        )
    tick_rate = context.tick_rate if context else LOGIC_HZ
    tick = tick_from_time_us(server_time_us, tick_rate)
    if not full:
        if base is None:
            raise KernelError(
                E_SEMANTIC,
                "v2 delta frame requires a base state",
                path="$.full_snapshot",
                source_version=version,
            )
        _check_delta_tick(base, tick, "$.server_time_us", version)
    elif base is not None and tick < base.tick:
        raise KernelError(
            E_SEMANTIC,
            "v2 server time moved backwards",
            path="$.server_time_us",
            source_version=version,
            details={"base_tick": base.tick, "frame_tick": tick},
        )
    state = _clone_base_state(base, context, match_id, version)
    units_data = _required(frame, "units", version)
    if not isinstance(units_data, list):
        raise KernelError(
            E_DECODE,
            "v2 units must be an array",
            path="$.units",
            source_version=version,
        )
    if full:
        state.units = {}
    for index, entry in enumerate(units_data):
        path = f"$.units[{index}]"
        if not isinstance(entry, dict):
            raise KernelError(
                E_DECODE,
                "v2 unit entry must be an object",
                path=path,
                source_version=version,
            )
        unit_id = _field_str(entry, "unit_id", path, version, required=True)
        assert unit_id is not None
        if full:
            for key in (
                "owner_id",
                "team",
                "position",
                "facing_mdeg",
                "health",
                "cooldown_ticks",
                "resource_units",
                "status",
            ):
                if key not in entry:
                    raise KernelError(
                        E_VER,
                        f"required full-snapshot field {key!r} is missing",
                        path=f"{path}.{key}",
                        source_version=version,
                        object_id=unit_id,
                    )
            existing = UnitState(
                unit_id=unit_id,
                owner_id="",
                team="",
                x=0,
                y=0,
                heading=0,
                hp=1,
                max_hp=1,
            )
        else:
            if unit_id not in state.units:
                raise KernelError(
                    E_SEMANTIC,
                    "v2 delta references an unknown unit",
                    path=f"{path}.unit_id",
                    source_version=version,
                    object_id=unit_id,
                )
            existing = state.units[unit_id].clone()

        if "position" in entry:
            existing.x, existing.y = _position(entry["position"], f"{path}.position", version)
        if "facing_mdeg" in entry:
            value = _field_int(entry, "facing_mdeg", path, version, required=full)
            if value is not None:
                existing.heading = value
        if "health" in entry:
            existing.hp, existing.max_hp = _health(entry["health"], f"{path}.health", version)
        if "cooldown_ticks" in entry:
            value = _field_int(entry, "cooldown_ticks", path, version, required=full)
            if value is not None:
                existing.cooldown = value
        if "resource_units" in entry:
            value = _field_int(entry, "resource_units", path, version, required=full)
            if value is not None:
                existing.resource = value
        if "owner_id" in entry:
            value = _field_str(entry, "owner_id", path, version, required=full)
            if value is not None:
                existing.owner_id = value
        if "team" in entry:
            value = _field_str(entry, "team", path, version, required=full)
            if value is not None:
                existing.team = value
        # v2's optional-status semantics are intentionally different from v1:
        # omission means an explicit zero, not "retain previous".
        status = _field_int(entry, "status", path, version, required=False)
        existing.status_mask = 0 if status is None else status
        state.units[unit_id] = existing

    if not state.units:
        raise KernelError(
            E_SEMANTIC,
            "decoded state has no units",
            path="$.units",
            source_version=version,
        )
    logical_clock = tick * (context.clock_hz if context else LOGIC_HZ)
    state = _finish_decoded_state(
        state,
        tick=tick,
        sequence=sequence,
        logical_clock=logical_clock,
        version=version,
        time_base="server_time_us",
    )
    expected_hash8 = state.snapshot_hash[:8]
    if content_hash8.lower() != expected_hash8:
        raise KernelError(
            E_DECODE,
            "v2 content_hash8 does not match decoded state",
            path="$.content_hash8",
            source_version=version,
            details={"declared": content_hash8, "computed": expected_hash8},
        )
    return DecodedFrame(
        state=state,
        source_version=version,
        source_sequence=sequence,
        source_time=server_time_us,
        time_base="server_time_us",
        was_full=full,
    )


def _v3_snapshot_unit(
    unit_id: str,
    value: Any,
    path: str,
    version: int,
) -> UnitState:
    if not isinstance(value, dict):
        raise KernelError(
            E_DECODE,
            "v3 snapshot unit must be an object",
            path=path,
            source_version=version,
            object_id=unit_id,
        )
    required = (
        "owner_id",
        "team",
        "position",
        "heading_mdeg",
        "health_current",
        "health_maximum",
        "cooldown_ticks",
        "resource_units",
        "status_mask",
    )
    for key in required:
        if key not in value:
            raise KernelError(
                E_VER,
                f"required v3 snapshot field {key!r} is missing",
                path=f"{path}.{key}",
                source_version=version,
                object_id=unit_id,
            )
    x, y = _position(value["position"], f"{path}.position", version)
    return UnitState(
        unit_id=unit_id,
        owner_id=_required_str(value, "owner_id", version, f"{path}.owner_id"),
        team=_required_str(value, "team", version, f"{path}.team"),
        x=x,
        y=y,
        heading=_field_int(value, "heading_mdeg", path, version, required=True) or 0,
        hp=_field_int(value, "health_current", path, version, required=True) or 0,
        max_hp=_field_int(value, "health_maximum", path, version, required=True) or 0,
        cooldown=_field_int(value, "cooldown_ticks", path, version, required=True) or 0,
        resource=_field_int(value, "resource_units", path, version, required=True) or 0,
        status_mask=_field_int(value, "status_mask", path, version, required=True) or 0,
    )


def _decode_v3(
    frame: dict[str, Any],
    base: AuthoritativeState | None,
    context: MatchContext | None,
) -> DecodedFrame:
    version = 3
    kind = _required(frame, "kind", version)
    if kind != "snapshot_delta":
        raise KernelError(
            E_DECODE,
            "v3 frame kind must be 'snapshot_delta'",
            path="$.kind",
            source_version=version,
        )
    sequence = _required_int(frame, "sequence", version, minimum=0)
    logical_clock = _required_int(frame, "logical_clock", version, minimum=0)
    clock_hz = context.clock_hz if context else LOGIC_HZ
    if logical_clock % clock_hz != 0:
        raise KernelError(
            E_SEMANTIC,
            f"logical_clock is not aligned to the {clock_hz} Hz logical time base",
            path="$.logical_clock",
            source_version=version,
            details={"logical_clock": logical_clock, "clock_hz": clock_hz},
        )
    tick = logical_clock // clock_hz
    if base is not None and logical_clock <= base.logical_clock:
        raise KernelError(
            E_SEMANTIC,
            "v3 logical clock did not advance monotonically",
            path="$.logical_clock",
            source_version=version,
            details={"base_clock": base.logical_clock, "frame_clock": logical_clock},
        )
    match_id = _required_str(frame, "match_id", version)
    snapshot_hash = _required_str(frame, "snapshot_hash", version)
    snapshot = _required(frame, "snapshot", version)
    delta = _required(frame, "delta", version)
    if not isinstance(snapshot, dict) or not snapshot:
        raise KernelError(
            E_DECODE,
            "v3 snapshot must be a non-empty object",
            path="$.snapshot",
            source_version=version,
        )
    if not isinstance(delta, list):
        raise KernelError(
            E_DECODE,
            "v3 delta must be an array",
            path="$.delta",
            source_version=version,
        )
    state = _clone_base_state(base, context, match_id, version)
    state.units = {}
    for unit_id in sorted(snapshot):
        if not isinstance(unit_id, str) or not unit_id:
            raise KernelError(
                E_DECODE,
                "v3 snapshot keys must be non-empty unit identifiers",
                path="$.snapshot",
                source_version=version,
            )
        state.units[unit_id] = _v3_snapshot_unit(
            unit_id,
            snapshot[unit_id],
            f"$.snapshot.{unit_id}",
            version,
        )
    for index, entry in enumerate(delta):
        path = f"$.delta[{index}]"
        if not isinstance(entry, dict):
            raise KernelError(
                E_DECODE,
                "v3 delta entry must be an object",
                path=path,
                source_version=version,
            )
        unit_id = _field_str(entry, "unit_id", path, version, required=True)
        assert unit_id is not None
        if unit_id not in state.units:
            raise KernelError(
                E_SEMANTIC,
                "v3 delta references a unit absent from the snapshot",
                path=f"{path}.unit_id",
                source_version=version,
                object_id=unit_id,
            )
        unit = state.units[unit_id]
        checks: list[tuple[str, Any, str]] = []
        if "position" in entry:
            dx, dy = _position(entry["position"], f"{path}.position", version)
            checks.extend(
                [
                    ("x", dx, "position.x"),
                    ("y", dy, "position.y"),
                ]
            )
        for wire_name, attr, converter in (
            ("heading_mdeg", "heading", int),
            ("health_current", "hp", int),
            ("health_maximum", "max_hp", int),
            ("cooldown_ticks", "cooldown", int),
            ("resource_units", "resource", int),
            ("status_mask", "status_mask", int),
        ):
            if wire_name in entry:
                value = entry[wire_name]
                if not is_int(value):
                    raise KernelError(
                        E_DECODE,
                        f"v3 delta field {wire_name!r} must be an integer",
                        path=f"{path}.{wire_name}",
                        source_version=version,
                        object_id=unit_id,
                    )
                checks.append((attr, converter(value), wire_name))
        if "owner_id" in entry:
            value = _field_str(entry, "owner_id", path, version, required=False)
            checks.append(("owner_id", value, "owner_id"))
        if "team" in entry:
            value = _field_str(entry, "team", path, version, required=False)
            checks.append(("team", value, "team"))
        for field_name, delta_value, wire_name in checks:
            snapshot_value = getattr(unit, field_name)
            if delta_value != snapshot_value:
                raise KernelError(
                    E_SEMANTIC,
                    "v3 delta conflicts with the complete snapshot",
                    path=f"{path}.{wire_name}",
                    source_version=version,
                    object_id=unit_id,
                    details={
                        "delta_value": delta_value,
                        "snapshot_value": snapshot_value,
                    },
                )

    state = _finish_decoded_state(
        state,
        tick=tick,
        sequence=sequence,
        logical_clock=logical_clock,
        version=version,
        time_base="logical_clock_120hz",
    )
    if snapshot_hash.lower() != state.snapshot_hash:
        raise KernelError(
            E_DECODE,
            "v3 snapshot_hash does not match decoded state",
            path="$.snapshot_hash",
            source_version=version,
            details={"declared": snapshot_hash, "computed": state.snapshot_hash},
        )
    return DecodedFrame(
        state=state,
        source_version=version,
        source_sequence=sequence,
        source_time=logical_clock,
        time_base="logical_clock_120hz",
        was_full=True,
    )


def decode_frame(
    payload: bytes | str | dict[str, Any],
    base: AuthoritativeState | None = None,
    context: MatchContext | None = None,
) -> DecodedFrame:
    frame = parse_json_payload(payload)
    raw_version = frame.get("protocol_version")
    if not is_int(raw_version) or raw_version not in SUPPORTED_VERSIONS:
        raise KernelError(
            E_VER,
            "unsupported or missing protocol_version",
            path="$.protocol_version",
            source_version=raw_version,
            details={"supported_versions": list(SUPPORTED_VERSIONS)},
        )
    version = int(raw_version)
    if version == 1:
        decoded = _decode_v1(frame, base, context)
    elif version == 2:
        decoded = _decode_v2(frame, base, context)
    else:
        decoded = _decode_v3(frame, base, context)
    decoded.frame_bytes = canonical_json_bytes(frame)
    return decoded


def _unit_equal_field(
    state: AuthoritativeState,
    previous: AuthoritativeState | None,
    unit: UnitState,
    field_name: str,
) -> bool:
    if previous is None or unit.unit_id not in previous.units:
        return False
    return getattr(previous.units[unit.unit_id], field_name) == getattr(unit, field_name)


def _encode_v1(
    state: AuthoritativeState,
    previous: AuthoritativeState | None,
) -> tuple[dict[str, Any], list[KernelIssue], list[dict[str, Any]]]:
    full = previous is None
    units = []
    for unit_id in sorted(state.units):
        unit = state.units[unit_id]
        entry: dict[str, Any] = {"id": unit_id}
        if full or not _unit_equal_field(state, previous, unit, "x") or not _unit_equal_field(
            state, previous, unit, "y"
        ):
            entry["p"] = [unit.x, unit.y]
        if full or not _unit_equal_field(state, previous, unit, "heading"):
            entry["rot"] = unit.heading
        if full or not _unit_equal_field(state, previous, unit, "hp"):
            entry["hp"] = unit.hp
        if full or not _unit_equal_field(state, previous, unit, "max_hp"):
            entry["hp_max"] = unit.max_hp
        if full or not _unit_equal_field(state, previous, unit, "cooldown"):
            entry["cd"] = unit.cooldown
        if full or not _unit_equal_field(state, previous, unit, "resource"):
            entry["mana"] = unit.resource
        if full or not _unit_equal_field(state, previous, unit, "status_mask"):
            entry["st"] = unit.status_mask
        if full or not _unit_equal_field(state, previous, unit, "owner_id"):
            entry["owner"] = unit.owner_id
        if full or not _unit_equal_field(state, previous, unit, "team"):
            entry["team"] = unit.team
        units.append(entry)
    frame = {
        "protocol_version": 1,
        "type": "sync",
        "sequence": state.sequence,
        "server_tick": state.tick,
        "full": full,
        "match": state.match_id,
        "units": units,
    }
    warnings = [
        warn_lossy(
            "$.logical_clock",
            "v1 cannot carry logical_clock; it is degraded to integer server_tick",
            source_version=1,
            details={"mapped_to": "$.server_tick", "strategy": "tick_div"},
        ),
        warn_lossy(
            "$.snapshot_hash",
            "v1 cannot carry the full snapshot hash; the receiver must recompute it",
            source_version=1,
            details={"mapped_to": "<recomputed>", "strategy": "omit_and_recompute"},
        ),
        warn_lossy(
            "$.source_time_base",
            "v1 has only a server-tick time base",
            source_version=1,
            details={"mapped_to": "$.server_tick", "strategy": "time_base_coercion"},
        ),
    ]
    mappings = [
        {
            "from_path": "$.logical_clock",
            "to_path": "$.server_tick",
            "strategy": "tick_div",
            "lossy": True,
        },
        {
            "from_path": "$.snapshot_hash",
            "to_path": "<recomputed>",
            "strategy": "omit_and_recompute",
            "lossy": True,
        },
        {
            "from_path": "$.source_time_base",
            "to_path": "$.server_tick",
            "strategy": "time_base_coercion",
            "lossy": True,
        },
    ]
    return frame, warnings, mappings


def _encode_v2(
    state: AuthoritativeState,
    previous: AuthoritativeState | None,
    context: MatchContext | None,
) -> tuple[dict[str, Any], list[KernelIssue], list[dict[str, Any]]]:
    full = previous is None
    tick_rate = context.tick_rate if context else LOGIC_HZ
    units = []
    for unit_id in sorted(state.units):
        unit = state.units[unit_id]
        entry: dict[str, Any] = {"unit_id": unit_id}
        if full or not (
            _unit_equal_field(state, previous, unit, "x")
            and _unit_equal_field(state, previous, unit, "y")
        ):
            entry["position"] = {"x": unit.x, "y": unit.y}
        if full or not _unit_equal_field(state, previous, unit, "heading"):
            entry["facing_mdeg"] = unit.heading
        if full or not (
            _unit_equal_field(state, previous, unit, "hp")
            and _unit_equal_field(state, previous, unit, "max_hp")
        ):
            entry["health"] = {"current": unit.hp, "maximum": unit.max_hp}
        if full or not _unit_equal_field(state, previous, unit, "cooldown"):
            entry["cooldown_ticks"] = unit.cooldown
        if full or not _unit_equal_field(state, previous, unit, "resource"):
            entry["resource_units"] = unit.resource
        if full or not _unit_equal_field(state, previous, unit, "owner_id"):
            entry["owner_id"] = unit.owner_id
        if full or not _unit_equal_field(state, previous, unit, "team"):
            entry["team"] = unit.team
        status_unchanged = _unit_equal_field(state, previous, unit, "status_mask")
        if full or unit.status_mask != 0 or not status_unchanged:
            entry["status"] = unit.status_mask
        units.append(entry)
    frame = {
        "protocol_version": 2,
        "kind": "sync_delta",
        "sequence": state.sequence,
        "server_time_us": time_us_for_tick(state.tick, tick_rate),
        "full_snapshot": full,
        "match_id": state.match_id,
        "content_hash8": state.snapshot_hash[:8],
        "units": units,
    }
    warnings = [
        warn_lossy(
            "$.logical_clock",
            "v2 cannot carry logical_clock; it is represented through integer microseconds",
            source_version=2,
            details={"mapped_to": "$.server_time_us", "strategy": "clock_to_microseconds"},
        ),
        warn_lossy(
            "$.snapshot_hash",
            "v2 carries only the first 8 hexadecimal characters of the state hash",
            source_version=2,
            details={"mapped_to": "$.content_hash8", "strategy": "hash_prefix_8"},
        ),
        warn_lossy(
            "$.source_time_base",
            "v2 has a microsecond time base rather than the 120 Hz logical clock",
            source_version=2,
            details={"mapped_to": "$.server_time_us", "strategy": "time_base_coercion"},
        ),
    ]
    mappings = [
        {
            "from_path": "$.logical_clock",
            "to_path": "$.server_time_us",
            "strategy": "clock_to_microseconds",
            "lossy": True,
        },
        {
            "from_path": "$.snapshot_hash",
            "to_path": "$.content_hash8",
            "strategy": "hash_prefix_8",
            "lossy": True,
        },
        {
            "from_path": "$.source_time_base",
            "to_path": "$.server_time_us",
            "strategy": "time_base_coercion",
            "lossy": True,
        },
    ]
    return frame, warnings, mappings


def _v3_delta_entry(
    unit: UnitState,
    previous: AuthoritativeState | None,
) -> dict[str, Any]:
    entry: dict[str, Any] = {"unit_id": unit.unit_id}
    if previous is None:
        return entry
    old = previous.units.get(unit.unit_id)
    if old is None:
        return entry
    if unit.x != old.x or unit.y != old.y:
        entry["position"] = {"x": unit.x, "y": unit.y}
    if unit.heading != old.heading:
        entry["heading_mdeg"] = unit.heading
    if unit.hp != old.hp:
        entry["health_current"] = unit.hp
    if unit.max_hp != old.max_hp:
        entry["health_maximum"] = unit.max_hp
    if unit.cooldown != old.cooldown:
        entry["cooldown_ticks"] = unit.cooldown
    if unit.resource != old.resource:
        entry["resource_units"] = unit.resource
    if unit.status_mask != old.status_mask:
        entry["status_mask"] = unit.status_mask
    if unit.owner_id != old.owner_id:
        entry["owner_id"] = unit.owner_id
    if unit.team != old.team:
        entry["team"] = unit.team
    return entry


def _encode_v3(
    state: AuthoritativeState,
    previous: AuthoritativeState | None,
) -> tuple[dict[str, Any], list[KernelIssue], list[dict[str, Any]]]:
    snapshot = {
        unit_id: {
            "owner_id": state.units[unit_id].owner_id,
            "team": state.units[unit_id].team,
            "position": {
                "x": state.units[unit_id].x,
                "y": state.units[unit_id].y,
            },
            "heading_mdeg": state.units[unit_id].heading,
            "health_current": state.units[unit_id].hp,
            "health_maximum": state.units[unit_id].max_hp,
            "cooldown_ticks": state.units[unit_id].cooldown,
            "resource_units": state.units[unit_id].resource,
            "status_mask": state.units[unit_id].status_mask,
        }
        for unit_id in sorted(state.units)
    }
    delta = [
        _v3_delta_entry(state.units[unit_id], previous)
        for unit_id in sorted(state.units)
    ]
    frame = {
        "protocol_version": 3,
        "kind": "snapshot_delta",
        "sequence": state.sequence,
        "logical_clock": state.logical_clock,
        "match_id": state.match_id,
        "snapshot_hash": state.snapshot_hash,
        "snapshot": snapshot,
        "delta": delta,
    }
    return frame, [], []


def encode_frame(
    state: AuthoritativeState,
    target_version: int,
    previous: AuthoritativeState | None = None,
    context: MatchContext | None = None,
) -> EncodeResult:
    if target_version not in SUPPORTED_VERSIONS:
        raise KernelError(
            E_VER,
            f"unsupported target protocol version {target_version!r}",
            path="$.target_version",
            source_version=target_version,
            details={"supported_versions": list(SUPPORTED_VERSIONS)},
        )
    if previous is not None:
        if previous.match_id != state.match_id:
            raise KernelError(
                E_SEMANTIC,
                "previous and current state belong to different matches",
                path="$.match_id",
            )
        if previous.tick != state.tick - 1:
            raise KernelError(
                E_SEQ,
                "previous state is not the immediately preceding tick",
                path="$.tick",
                details={"previous_tick": previous.tick, "current_tick": state.tick},
            )
    state.validate()
    if state.snapshot_hash:
        computed_hash = state.compute_hash()
        if computed_hash != state.snapshot_hash:
            raise KernelError(
                E_SEMANTIC,
                "state snapshot hash is stale and cannot be encoded",
                path="$.snapshot_hash",
                details={
                    "declared": state.snapshot_hash,
                    "computed": computed_hash,
                },
            )
    else:
        state = state.clone().rehash()
    if target_version == 1:
        frame, warnings, mappings = _encode_v1(state, previous)
    elif target_version == 2:
        frame, warnings, mappings = _encode_v2(state, previous, context)
    else:
        frame, warnings, mappings = _encode_v3(state, previous)
    payload = canonical_json_bytes(frame)
    # Encoding is pure: a repeat call must produce exactly the same bytes even
    # when callers mutate insertion order in equivalent dictionaries.
    if canonical_json_bytes(dict(reversed(list(frame.items())))) != payload:
        raise KernelError(E_DECODE, "internal canonical encoding instability", path="$")
    return EncodeResult(
        frame=frame,
        payload=payload,
        target_version=target_version,
        warnings=warnings,
        mappings=mappings,
    )

