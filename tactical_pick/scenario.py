"""Deterministic scenario execution shared by generation and verification."""

from __future__ import annotations

import threading
from dataclasses import dataclass
from typing import Any, Mapping

from .camera import Camera
from .canonical import canonical_json_bytes
from .errors import E_CANCELLED, E_SCENE_INVALID, PickError, fail
from .picking import PickResult, pick_from_store
from .placement import PlacementResult, check_placement
from .scene_io import decode_scene
from .trace import TraceRecorder
from .versioning import VersionStore


@dataclass(frozen=True)
class RunOutput:
    sample_id: str
    store: VersionStore
    recorder: TraceRecorder
    expected: dict[str, Any]
    final_version: int
    final_hash: str

    @property
    def trace_bytes(self) -> bytes:
        return self.recorder.to_bytes()

    @property
    def expected_bytes(self) -> bytes:
        return canonical_json_bytes(self.expected) + b"\n"


def _cancelled(cancel_event: threading.Event | None) -> None:
    if cancel_event is not None and cancel_event.is_set():
        raise PickError(
            code=E_CANCELLED,
            message="scenario execution was cooperatively cancelled",
            location={"field": "cancel_event"},
        )


def _event_version(event: Mapping[str, Any]) -> int | None:
    value = event.get("expected_version")
    if value is None:
        return None
    if isinstance(value, bool) or not isinstance(value, int):
        fail(
            E_SCENE_INVALID,
            "scenario expected_version must be an integer",
            field="scenario.events.expected_version",
        )
    return value


def run_scenario(
    scene_bytes: bytes,
    scenario: Mapping[str, Any],
    *,
    cancel_event: threading.Event | None = None,
) -> RunOutput:
    if scenario.get("schema") != 1:
        fail(
            E_SCENE_INVALID,
            "scenario schema is missing or unsupported",
            field="scenario.schema",
        )
    sample_id = str(scenario.get("sample_id", "sample"))
    scene = decode_scene(scene_bytes, source=f"scenario:{sample_id}.scene")
    store = VersionStore(scene)
    camera = Camera.from_dict(scenario["camera"]).validate()
    recorder = TraceRecorder()
    events = scenario.get("events")
    if not isinstance(events, list) or not events:
        fail(
            E_SCENE_INVALID,
            "scenario must contain a non-empty event list",
            field="scenario.events",
        )
    expected_events: list[dict[str, Any]] = []
    for index, event in enumerate(events):
        _cancelled(cancel_event)
        if not isinstance(event, Mapping):
            fail(
                E_SCENE_INVALID,
                "scenario event must be an object",
                field=f"scenario.events[{index}]",
            )
        kind = event.get("type")
        label = str(event.get("label", f"event-{index}"))
        if kind == "batch":
            operations = event.get("ops")
            if not isinstance(operations, list):
                fail(
                    E_SCENE_INVALID,
                    "batch event requires an ops list",
                    field=f"scenario.events[{index}].ops",
                )
            base_version = event.get("base_version")
            if base_version is not None and (
                isinstance(base_version, bool) or not isinstance(base_version, int)
            ):
                fail(
                    E_SCENE_INVALID,
                    "batch event base_version must be an integer",
                    field=f"scenario.events[{index}].base_version",
                )
            snapshot = store.apply_batch(
                operations,
                expected_version=base_version,
            )
            expected_events.append(
                {
                    "event_index": index,
                    "type": "batch",
                    "label": label,
                    "ok": True,
                    "base_version": base_version,
                    "result": snapshot.version_key(),
                }
            )
            continue
        if kind == "pick":
            result: PickResult = pick_from_store(
                store,
                camera,
                event["screen"],
                expected_version=_event_version(event),
            )
            trace_record = recorder.append(result, camera, label=label)
            expected_events.append(
                {
                    "event_index": index,
                    "type": "pick",
                    "label": label,
                    "trace_seq": trace_record["seq"],
                    "result": result.to_dict(),
                }
            )
            continue
        if kind == "placement":
            placement_result: PlacementResult = check_placement(
                store,
                event["vertices"],
                expected_version=_event_version(event),
            )
            expected_events.append(
                {
                    "event_index": index,
                    "type": "placement",
                    "label": label,
                    "result": placement_result.to_dict(),
                }
            )
            continue
        fail(
            E_SCENE_INVALID,
            "scenario event type is unknown",
            field=f"scenario.events[{index}].type",
            actual=str(kind),
        )
    final = store.current_snapshot()
    expected = {
        "schema": 1,
        "sample_id": sample_id,
        "initial_version": 1,
        "final_version": final.version,
        "final_hash": final.content_hash,
        "trace_chain_head": recorder.chain_head,
        "trace_record_count": len(recorder.records),
        "events": expected_events,
    }
    return RunOutput(
        sample_id=sample_id,
        store=store,
        recorder=recorder,
        expected=expected,
        final_version=final.version,
        final_hash=final.content_hash,
    )
