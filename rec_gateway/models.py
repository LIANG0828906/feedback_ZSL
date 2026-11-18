from __future__ import annotations

import hashlib
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import onnxruntime as ort

from rec_gateway.contracts import Manifest, TensorSpec, load_manifest
from rec_gateway.errors import ModelLoadError


ORT_DTYPE_NAMES = {
    "tensor(float)": "float32",
}


def utc_now() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


@dataclass
class VersionRuntime:
    status: str = "loading"
    manifest: Manifest | None = None
    session: ort.InferenceSession | None = None
    error_code: str | None = None
    error_message: str | None = None
    error_details: dict[str, Any] | None = None
    loaded_at: str | None = None


@dataclass(frozen=True)
class ReadyModel:
    version: str
    manifest: Manifest
    session: ort.InferenceSession


class ModelManager:
    def __init__(self, model_dir: str | Path, versions: tuple[str, ...]) -> None:
        self.model_dir = Path(model_dir).resolve()
        self.versions = tuple(dict.fromkeys(versions))
        self._lock = threading.RLock()
        self._load_locks = {version: threading.Lock() for version in self.versions}
        self._states = {version: VersionRuntime() for version in self.versions}

    def _set_state(self, version: str, state: VersionRuntime) -> None:
        with self._lock:
            self._states[version] = state

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            output: dict[str, Any] = {}
            for version in sorted(self.versions):
                state = self._states[version]
                manifest = state.manifest
                output[version] = {
                    "version": version,
                    "status": state.status,
                    "protocol_version": (
                        manifest.protocol_version if manifest else version
                    ),
                    "feature_dimension": (
                        manifest.feature_dimension if manifest else None
                    ),
                    "outputs": (
                        [
                            {
                                "name": spec.name,
                                "shape": list(spec.shape),
                                "dtype": spec.dtype,
                            }
                            for spec in manifest.output_specs
                        ]
                        if manifest
                        else []
                    ),
                    "response_kind": manifest.response_kind if manifest else None,
                    "error_code": state.error_code,
                    "error_message": state.error_message,
                    "error_details": state.error_details or {},
                    "loaded_at": state.loaded_at,
                }
            return output

    def get_state(self, version: str) -> dict[str, Any] | None:
        with self._lock:
            if version not in self._states:
                return None
            state = self._states[version]
            return {
                "status": state.status,
                "error_code": state.error_code,
            }

    def get_ready(self, version: str) -> ReadyModel | None:
        with self._lock:
            state = self._states.get(version)
            if (
                state is None
                or state.status != "ready"
                or state.manifest is None
                or state.session is None
            ):
                return None
            return ReadyModel(version, state.manifest, state.session)

    def is_ready(self, version: str) -> bool:
        return self.get_ready(version) is not None

    def available_versions(self) -> list[str]:
        return [version for version in self.versions if self.is_ready(version)]

    def load_all(self) -> None:
        workers = max(1, min(len(self.versions), 2))
        with ThreadPoolExecutor(max_workers=workers) as executor:
            futures = [executor.submit(self.load_version, version) for version in self.versions]
            for future in futures:
                future.result()

    def load_version(self, version: str) -> None:
        if version not in self._states:
            raise KeyError(version)
        load_lock = self._load_locks[version]
        if not load_lock.acquire(blocking=False):
            return
        try:
            with self._lock:
                previous = self._states[version]
                self._states[version] = VersionRuntime(
                    status="loading",
                    manifest=previous.manifest,
                    session=previous.session,
                    loaded_at=previous.loaded_at,
                )
            try:
                manifest, session = self._open_validated(version)
            except ModelLoadError as exc:
                self._set_state(
                    version,
                    VersionRuntime(
                        status="failed",
                        error_code=exc.code,
                        error_message=exc.message,
                        error_details=exc.details,
                        loaded_at=utc_now(),
                    ),
                )
                return
            except Exception:
                self._set_state(
                    version,
                    VersionRuntime(
                        status="failed",
                        error_code="MODEL_LOAD_FAILED",
                        error_message="ONNX Runtime failed to load the model",
                        error_details={},
                        loaded_at=utc_now(),
                    ),
                )
                return
            self._set_state(
                version,
                VersionRuntime(
                    status="ready",
                    manifest=manifest,
                    session=session,
                    loaded_at=utc_now(),
                ),
            )
        finally:
            load_lock.release()

    def reload_async(self, version: str) -> bool:
        if version not in self._states:
            raise KeyError(version)
        load_lock = self._load_locks[version]
        if not load_lock.acquire(blocking=False):
            return False
        load_lock.release()
        with self._lock:
            previous = self._states[version]
            self._states[version] = VersionRuntime(
                status="loading",
                manifest=previous.manifest,
                session=previous.session,
                loaded_at=previous.loaded_at,
            )
        thread = threading.Thread(
            target=self.load_version,
            args=(version,),
            name=f"model-reload-{version}",
            daemon=True,
        )
        thread.start()
        return True

    def _open_validated(
        self, version: str
    ) -> tuple[Manifest, ort.InferenceSession]:
        version_dir = self.model_dir / version
        manifest = load_manifest(version_dir, expected_protocol=version)
        model_path = (version_dir / manifest.model_file).resolve()
        try:
            model_path.relative_to(version_dir.resolve())
        except ValueError:
            raise ModelLoadError(
                "MANIFEST_INVALID", "model_file escapes the version directory"
            )
        if not model_path.is_file():
            raise ModelLoadError("MODEL_FILE_MISSING", "model file is missing")
        if manifest.model_sha256:
            digest = hashlib.sha256(model_path.read_bytes()).hexdigest()
            if digest != manifest.model_sha256:
                raise ModelLoadError(
                    "MODEL_HASH_MISMATCH",
                    "model sha256 does not match the manifest",
                    {"declared": manifest.model_sha256, "actual": digest},
                )

        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
        options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
        options.enable_mem_pattern = True
        options.enable_cpu_mem_arena = False
        try:
            session = ort.InferenceSession(
                str(model_path),
                sess_options=options,
                providers=["CPUExecutionProvider"],
            )
        except Exception:
            raise ModelLoadError(
                "MODEL_RUNTIME_LOAD_FAILED", "ONNX Runtime could not load the model"
            )

        self._validate_signature(manifest, session)
        return manifest, session

    def _validate_signature(
        self, manifest: Manifest, session: ort.InferenceSession
    ) -> None:
        inputs = session.get_inputs()
        if len(inputs) != 1:
            raise ModelLoadError(
                "MODEL_INPUT_CONTRACT_MISMATCH",
                "model must expose exactly one input",
                {"model_inputs": len(inputs), "declared_inputs": 1},
            )
        self._validate_tensor(manifest.input_spec, inputs[0], "input")

        actual_outputs = {output.name: output for output in session.get_outputs()}
        expected_outputs = {spec.name: spec for spec in manifest.output_specs}
        if set(actual_outputs) != set(expected_outputs):
            raise ModelLoadError(
                "MODEL_OUTPUT_CONTRACT_MISMATCH",
                "model output names do not match the manifest",
                {
                    "expected_outputs": sorted(expected_outputs),
                    "actual_outputs": sorted(actual_outputs),
                },
            )
        for output_spec in manifest.output_specs:
            self._validate_tensor(
                output_spec, actual_outputs[output_spec.name], "output"
            )

    def _validate_tensor(
        self,
        declared: TensorSpec,
        actual: Any,
        kind: str,
    ) -> None:
        if actual.name != declared.name:
            raise ModelLoadError(
                f"MODEL_{kind.upper()}_CONTRACT_MISMATCH",
                f"model {kind} name does not match the manifest",
                {
                    "expected_name": declared.name,
                    "actual_name": actual.name,
                },
            )
        actual_dtype = ORT_DTYPE_NAMES.get(actual.type, actual.type)
        if actual_dtype != declared.dtype:
            raise ModelLoadError(
                f"MODEL_{kind.upper()}_CONTRACT_MISMATCH",
                f"model {kind} dtype does not match the manifest",
                {
                    "tensor": declared.name,
                    "expected_dtype": declared.dtype,
                    "actual_dtype": actual_dtype,
                },
            )
        actual_shape = list(actual.shape)
        if len(actual_shape) != len(declared.shape):
            raise ModelLoadError(
                f"MODEL_{kind.upper()}_CONTRACT_MISMATCH",
                f"model {kind} rank does not match the manifest",
                {
                    "tensor": declared.name,
                    "expected_shape": list(declared.shape),
                    "actual_shape": actual_shape,
                },
            )
        for position, declared_dimension in enumerate(declared.shape):
            if isinstance(declared_dimension, int):
                actual_dimension = actual_shape[position]
                if actual_dimension != declared_dimension:
                    raise ModelLoadError(
                        f"MODEL_{kind.upper()}_CONTRACT_MISMATCH",
                        f"model {kind} dimension does not match the manifest",
                        {
                            "tensor": declared.name,
                            "dimension_index": position,
                            "expected_dimension": declared_dimension,
                            "actual_dimension": actual_dimension,
                        },
                    )
