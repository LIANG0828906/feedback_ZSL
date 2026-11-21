from __future__ import annotations

import hashlib
import os
import shutil
import signal
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

from .errors import E_IO, E_SNAPSHOT_INVALID, BusinessError
from .models import AnalysisState
from .util import canonical_json_bytes, load_json, write_bytes_atomic, write_json_atomic


class SnapshotStore:
    def __init__(self, root: Path) -> None:
        self.root = root
        self.snapshots = root / "snapshots"
        self.current_path = root / "CURRENT"
        self.snapshots.mkdir(parents=True, exist_ok=True)

    def _verify_directory(
        self, directory: Path
    ) -> Tuple[Dict[str, Any], bytes, bytes, bytes, Dict[str, Any]]:
        marker = directory / "COMMITTED"
        if not marker.is_file():
            raise BusinessError(
                E_SNAPSHOT_INVALID,
                "snapshot is not committed",
                details={"snapshot_id": directory.name},
            )
        metadata = load_json(marker)
        result_path = directory / "result.json"
        route_path = directory / "route_matrix.json"
        calibration_path = directory / "calibration.json"
        for path in (result_path, route_path, calibration_path):
            if not path.is_file():
                raise BusinessError(
                    E_SNAPSHOT_INVALID,
                    "committed snapshot is missing a required file",
                    details={"snapshot_id": directory.name, "file": path.name},
                )
        result_bytes = result_path.read_bytes()
        route_bytes = route_path.read_bytes()
        calibration_bytes = calibration_path.read_bytes()
        checks = {
            "result_sha256": hashlib.sha256(result_bytes).hexdigest(),
            "route_matrix_sha256": hashlib.sha256(route_bytes).hexdigest(),
            "calibration_sha256": hashlib.sha256(calibration_bytes).hexdigest(),
        }
        if metadata.get("state_hash") != checks["result_sha256"]:
            raise BusinessError(
                E_SNAPSHOT_INVALID,
                "committed state hash does not match result bytes",
                details={"snapshot_id": directory.name, "field": "state_hash"},
            )
        if metadata.get("snapshot_id") != directory.name:
            raise BusinessError(
                E_SNAPSHOT_INVALID,
                "committed snapshot id does not match its directory",
                details={"snapshot_id": directory.name},
            )
        for key, actual in checks.items():
            if metadata.get(key) != actual:
                raise BusinessError(
                    E_SNAPSHOT_INVALID,
                    "committed snapshot content hash mismatch",
                    details={"snapshot_id": directory.name, "field": key},
                )
        import json

        result = json.loads(result_bytes.decode("utf-8"))
        return metadata, result_bytes, route_bytes, calibration_bytes, result

    def commit(self, state: AnalysisState) -> Dict[str, Any]:
        result_bytes = canonical_json_bytes(state.to_dict())
        snapshot_id = hashlib.sha256(result_bytes).hexdigest()[:24]
        final_dir = self.snapshots / snapshot_id
        route_bytes = canonical_json_bytes(list(state.route_matrix))
        calibration_bytes = canonical_json_bytes(state.calibration)
        metadata = {
            "schema_version": 1,
            "snapshot_id": snapshot_id,
            "state_hash": state.content_hash_value,
            "result_sha256": hashlib.sha256(result_bytes).hexdigest(),
            "route_matrix_sha256": hashlib.sha256(route_bytes).hexdigest(),
            "calibration_sha256": hashlib.sha256(calibration_bytes).hexdigest(),
            "status": state.status,
        }
        if final_dir.is_dir() and (final_dir / "COMMITTED").is_file():
            self._verify_directory(final_dir)
        else:
            temp_dir = self.snapshots / f".tmp-{os.getpid()}-{snapshot_id}"
            if temp_dir.exists():
                shutil.rmtree(temp_dir)
            temp_dir.mkdir(parents=True)
            try:
                write_bytes_atomic(temp_dir / "result.json", result_bytes)
                write_bytes_atomic(temp_dir / "route_matrix.json", route_bytes)
                write_bytes_atomic(
                    temp_dir / "calibration.json", calibration_bytes
                )
                write_json_atomic(temp_dir / "metadata.json", metadata)
                if os.environ.get("STUDIO_CALIBRATOR_TEST_CRASH_AT") == "before-commit":
                    os.kill(os.getpid(), signal.SIGKILL)
                write_bytes_atomic(
                    temp_dir / "COMMITTED", canonical_json_bytes(metadata)
                )
                if final_dir.exists():
                    shutil.rmtree(final_dir)
                os.replace(temp_dir, final_dir)
                dir_fd = os.open(str(self.snapshots), os.O_RDONLY)
                try:
                    os.fsync(dir_fd)
                finally:
                    os.close(dir_fd)
            finally:
                if temp_dir.exists():
                    shutil.rmtree(temp_dir)
        write_bytes_atomic(self.current_path, (snapshot_id + "\n").encode("ascii"))
        return {
            "snapshot_id": snapshot_id,
            "state_hash": state.content_hash_value,
            "result_sha256": metadata["result_sha256"],
            "route_matrix_sha256": metadata["route_matrix_sha256"],
            "calibration_sha256": metadata["calibration_sha256"],
            "status": state.status,
        }

    def load_current(self) -> Dict[str, Any]:
        if not self.current_path.is_file():
            raise BusinessError(E_IO, "no committed snapshot pointer exists")
        try:
            snapshot_id = self.current_path.read_text(encoding="ascii").strip()
        except OSError as exc:
            raise BusinessError(
                E_SNAPSHOT_INVALID, f"cannot read CURRENT: {exc}"
            ) from exc
        if not snapshot_id or "/" in snapshot_id or snapshot_id.startswith("."):
            raise BusinessError(
                E_SNAPSHOT_INVALID,
                "CURRENT does not contain a safe snapshot id",
                details={"snapshot_id": snapshot_id},
            )
        directory = self.snapshots / snapshot_id
        if not directory.is_dir():
            raise BusinessError(
                E_SNAPSHOT_INVALID,
                "CURRENT points to a missing snapshot",
                details={"snapshot_id": snapshot_id},
            )
        metadata, result_bytes, route_bytes, calibration_bytes, result = (
            self._verify_directory(directory)
        )
        return {
            "snapshot_id": snapshot_id,
            "metadata": metadata,
            "state": result,
            "result_bytes": result_bytes,
            "route_matrix_bytes": route_bytes,
            "calibration_bytes": calibration_bytes,
        }

    def committed_ids(self) -> list[str]:
        ids = []
        for child in sorted(self.snapshots.iterdir()):
            if child.is_dir() and not child.name.startswith("."):
                if (child / "COMMITTED").is_file():
                    ids.append(child.name)
        return ids
