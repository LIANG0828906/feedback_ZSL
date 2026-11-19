"""ONNX model registry with per-version loading state machine."""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from pathlib import Path

from .errors import ErrorCode, GatewayError
from .protocol import ProtocolSpec


STATE_LOADING = "loading"
STATE_READY = "ready"
STATE_FAILED = "failed"


@dataclass
class ModelState:
    version: str
    protocol_version: str
    model_file: str
    state: str = STATE_LOADING
    loaded_at: float | None = None
    last_error: str | None = None
    load_attempts: int = 0
    session: object | None = field(default=None, repr=False)

    def snapshot(self) -> dict:
        return {
            "load_attempts": self.load_attempts,
            "loaded_at": self.loaded_at,
            "last_error": self.last_error,
            "model_file": self.model_file,
            "protocol_version": self.protocol_version,
            "state": self.state,
            "version": self.version,
        }


class ModelRegistry:
    """Loads and serves ONNX sessions; one version's failure never blocks another."""

    def __init__(self, model_dir: str | Path, specs: dict[str, ProtocolSpec]):
        self._model_dir = Path(model_dir)
        self._specs = specs
        self._states: dict[str, ModelState] = {}
        self._sessions: dict[str, object] = {}
        self._locks = {version: threading.Lock() for version in specs}
        for version, spec in specs.items():
            self._states[version] = ModelState(
                version=version,
                protocol_version=spec.version,
                model_file=spec.model_file,
            )

    def load_blocking(self, version: str) -> None:
        """Load one version synchronously (used at startup and by reload)."""

        spec = self._specs[version]
        with self._locks[version]:
            state = self._states[version]
            state.state = STATE_LOADING
            state.load_attempts += 1
            try:
                import onnxruntime as ort

                path = self._model_dir / spec.model_file
                if not path.exists():
                    raise FileNotFoundError(f"model file not found: {spec.model_file}")
                session = ort.InferenceSession(
                    str(path), providers=["CPUExecutionProvider"]
                )
                input_shape = session.get_inputs()[0].shape
                expected_cols = len(spec.columns)
                if len(input_shape) != 2 or input_shape[1] != expected_cols:
                    raise RuntimeError(
                        f"model input shape {input_shape} does not match "
                        f"{expected_cols} declared columns"
                    )
            except Exception as exc:  # noqa: BLE001 - state must capture any failure
                state.state = STATE_FAILED
                state.last_error = f"{type(exc).__name__}: {exc}"
                state.session = None
                self._sessions.pop(version, None)
                return

            state.session = session
            state.state = STATE_READY
            state.last_error = None
            state.loaded_at = time.time()
            self._sessions[version] = session

    def reload(self, version: str) -> ModelState:
        self.load_blocking(version)
        return self._states[version]

    def refresh_specs(self, specs: dict[str, ProtocolSpec]) -> None:
        """Pick up mapping-file changes before a reload attempt."""

        for version, spec in specs.items():
            with self._locks[version]:
                self._specs[version] = spec
                state = self._states.get(version)
                if state is not None:
                    state.protocol_version = spec.version
                    state.model_file = spec.model_file

    def get_ready_session(self, version: str):
        state = self._states[version]
        if state.state == STATE_FAILED:
            raise GatewayError(
                ErrorCode.MODEL_LOAD_FAILED,
                f"model {version} failed to load: {state.last_error}",
                field=f"model.{version}",
            )
        if state.state != STATE_READY or state.session is None:
            raise GatewayError(
                ErrorCode.MODEL_LOADING,
                f"model {version} is still loading",
                field=f"model.{version}",
            )
        return state.session

    def is_ready(self, version: str) -> bool:
        return self._states[version].state == STATE_READY

    def state_of(self, version: str) -> ModelState:
        return self._states[version]

    def snapshots(self) -> dict[str, dict]:
        return {version: state.snapshot() for version, state in sorted(self._states.items())}

    def wait_until_ready(self, version: str, timeout: float) -> None:
        """Block until the version becomes ready, fails, or the timeout elapses."""

        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            state = self._states[version]
            if state.state == STATE_READY:
                return
            if state.state == STATE_FAILED:
                raise GatewayError(
                    ErrorCode.MODEL_LOAD_FAILED,
                    f"model {version} failed to load: {state.last_error}",
                    field=f"model.{version}",
                )
            time.sleep(min(0.01, max(0.0, deadline - time.monotonic())))
        raise GatewayError(
            ErrorCode.READY_WAIT_TIMEOUT,
            f"model {version} not ready after {timeout * 1000:.0f}ms",
            field=f"model.{version}",
        )
