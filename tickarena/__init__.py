"""TickArena: deterministic local arena simulation and replay verification."""

from .checkpoint import CheckpointStore, ResumeInfo
from .codes import (
    E_ACTION,
    E_ARG,
    E_BUFFER,
    E_CANCELLED,
    E_COOLDOWN,
    E_DIVERGE,
    E_FINISHED,
    E_ITEM,
    E_MEMORY,
    E_MORE,
    E_MOVE,
    E_NUM,
    E_OK,
    E_ORDER,
    E_PAUSED,
    E_RANGE,
    E_REPLAY,
    E_RES,
    E_STATE,
    E_UNIT,
    Result,
)
from .compare import compare_modes, run_mode
from .concurrency import BoundedParallel
from .engine import BatchResult, Engine, EngineLimits, StepResult
from .events import InMemoryEventSource, JsonlEventSource, RawEvent
from .generator import (
    generate_events,
    make_initial_state,
    write_event_file,
    write_generated_match,
)
from .replay import ReplayRecorder, load_and_validate_replay, iter_replay_body
from .verifier import VerificationReport, verify_replay

__all__ = [
    "BatchResult",
    "BoundedParallel",
    "CheckpointStore",
    "Engine",
    "EngineLimits",
    "InMemoryEventSource",
    "JsonlEventSource",
    "RawEvent",
    "ReplayRecorder",
    "Result",
    "ResumeInfo",
    "StepResult",
    "VerificationReport",
    "compare_modes",
    "generate_events",
    "iter_replay_body",
    "load_and_validate_replay",
    "make_initial_state",
    "run_mode",
    "verify_replay",
    "write_event_file",
    "write_generated_match",
    "E_ACTION",
    "E_ARG",
    "E_BUFFER",
    "E_CANCELLED",
    "E_COOLDOWN",
    "E_DIVERGE",
    "E_FINISHED",
    "E_ITEM",
    "E_MEMORY",
    "E_MORE",
    "E_MOVE",
    "E_NUM",
    "E_OK",
    "E_ORDER",
    "E_PAUSED",
    "E_RANGE",
    "E_REPLAY",
    "E_RES",
    "E_STATE",
    "E_UNIT",
]

