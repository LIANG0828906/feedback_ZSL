"""Acceptance checks over the deterministic local sample artifacts."""

from __future__ import annotations

import json
import shutil
import struct
import tempfile
import threading
import time
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .buffers import BufferStore
from .errors import ErrorCode, VolumeRenderError
from .params import RenderParameters
from .preview import PreviewController
from .renderer import RenderCancelled, rebind_result, render_offscreen
from .snapshot import SnapshotManager, load_snapshot, recover_from_snapshot
from .synthetic import DEFAULT_SEED, baseline_parameters, generate_samples
from .tasks import OffscreenRenderService
from .trace import DeterministicTrace, read_trace
from .volume import load_volume


def _safe(value: Any) -> Any:
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, tuple):
        return [_safe(item) for item in value]
    if isinstance(value, list):
        return [_safe(item) for item in value]
    if isinstance(value, dict):
        return {str(key): _safe(item) for key, item in value.items()}
    return value


class _Checks:
    def __init__(self) -> None:
        self.records: list[dict[str, Any]] = []

    def run(self, name: str, function: Callable[[], Any]) -> Any | None:
        try:
            details = function()
        except Exception as exc:
            if isinstance(exc, VolumeRenderError):
                error = exc.to_dict()
            else:
                error = {"code": type(exc).__name__, "message": str(exc)}
            self.records.append(
                {
                    "name": name,
                    "passed": False,
                    "error": _safe(error),
                }
            )
            return None
        self.records.append(
            {
                "name": name,
                "passed": True,
                "details": _safe(details or {}),
            }
        )
        return details


def _expect_error(code: ErrorCode, function: Callable[[], Any]) -> dict[str, Any]:
    try:
        function()
    except VolumeRenderError as exc:
        if exc.code != code:
            raise AssertionError(f"expected {code.value}, got {exc.code.value}") from exc
        return {"code": exc.code.value, "field": exc.field, "details": exc.details}
    raise AssertionError(f"expected {code.value}, no error was raised")


def _load_levels(root: Path) -> tuple[dict[str, Any], BufferStore]:
    manifest = json.loads((root / "samples" / "samples.json").read_text(encoding="utf-8"))
    records = {str(level["dims"][0]): level for level in manifest["levels"]}
    return records, BufferStore(root / "buffers")


def _copy_buffer_tree(source: BufferStore, target: BufferStore, parameter_hash: str) -> None:
    target.root.mkdir(parents=True, exist_ok=True)
    for suffix in (".json", ".rgba", ".mask"):
        source_path = source.root / f"{parameter_hash}{suffix}"
        if source_path.is_file():
            shutil.copy2(source_path, target.root / source_path.name)


def _level_checks(
    checks: _Checks,
    root: Path,
    record: dict[str, Any],
    buffer_store: BufferStore,
) -> None:
    level = int(record["dims"][0])
    volume = load_volume(root / "samples" / str(record["path"]))
    params = baseline_parameters(volume)
    snapshot = load_snapshot(root / "snapshots" / str(record["baseline_snapshot"]))
    stored = buffer_store.load_verified(
        str(record["baseline_buffer_name"]),
        expected_parameter_hash=params.parameter_hash,
    )
    baseline_result = stored.result

    def header_check() -> dict[str, Any]:
        assert volume.dims == tuple(record["dims"])
        assert volume.header.mask_offset == int(record["mask_offset"])
        assert volume.content_sha256 == record["content_sha256"]
        return {
            "dims": list(volume.dims),
            "spacing": list(volume.spacing),
            "scalar_type": volume.header.scalar_type,
            "mask_offset": volume.header.mask_offset,
            "content_sha256": volume.content_sha256,
        }

    checks.run(f"{level}:header_parse", header_check)

    def stale_metadata_rejected() -> dict[str, Any]:
        assert stored.result.buffer_hash == record["baseline_buffer_hash"]
        assert stored.manifest["parameter_hash"] == params.parameter_hash
        with tempfile.TemporaryDirectory() as directory:
            temp_store = BufferStore(Path(directory))
            _copy_buffer_tree(buffer_store, temp_store, params.parameter_hash)
            manifest_path = temp_store.root / f"{params.parameter_hash}.json"
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            del manifest["parameter_hash"]
            manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            error = _expect_error(
                ErrorCode.E_PARAM_STALE,
                lambda: temp_store.load_verified(params.parameter_hash),
            )
        return {
            "baseline_buffer_hash": stored.result.buffer_hash,
            "tamper_error": error,
        }

    checks.run(f"{level}:param_stale_rejection", stale_metadata_rejected)

    def deterministic_repeat() -> dict[str, Any]:
        repeat = render_offscreen(volume, params)
        assert repeat.rgba_bytes == baseline_result.rgba_bytes
        assert repeat.buffer_hash == baseline_result.buffer_hash
        return {
            "parameter_hash": repeat.parameter_hash,
            "buffer_hash": repeat.buffer_hash,
            "byte_count": len(repeat.rgba_bytes),
        }

    checks.run(f"{level}:offscreen_byte_repeatability", deterministic_repeat)

    pending = params.with_changes(
        window_center=params.window_center + 90.0,
        window_width=params.window_width * 1.14,
    )

    def preview_alignment() -> dict[str, Any]:
        controller = PreviewController()
        controller.commit(params, baseline_result)
        controller.set_pending(pending)
        during = controller.state()
        assert during["displayed_parameter_hash"] == params.parameter_hash
        assert during["displayed_buffer_hash"] == baseline_result.buffer_hash
        assert during["pending_parameter_hash"] == pending.parameter_hash
        assert during["pending_is_uncommitted"] is True
        new_result = render_offscreen(volume, pending)
        controller.commit(pending, new_result)
        after = controller.state()
        assert after["displayed_parameter_hash"] == pending.parameter_hash
        assert after["displayed_buffer_hash"] == new_result.buffer_hash
        assert after["pending_is_uncommitted"] is False
        mismatched = rebind_result(new_result, params.parameter_hash)
        error = _expect_error(
            ErrorCode.E_PREVIEW_MISMATCH,
            lambda: controller.commit(params, mismatched),
        )
        assert controller.displayed_parameter_hash == pending.parameter_hash
        return {
            "during_drag": during,
            "after_commit": after,
            "mismatch_error": error,
            "new_buffer_hash": new_result.buffer_hash,
        }

    checks.run(f"{level}:preview_offscreen_alignment", preview_alignment)

    def mask_consistency() -> dict[str, Any]:
        masked = render_offscreen(volume, params)
        without_params = params.with_changes(mask_enabled=False, mask_hash="")
        without = render_offscreen(volume, without_params)
        changed = np.any(masked.color != without.color, axis=2)
        coverage = masked.mask_coverage
        assert coverage is not None
        outside = int(np.count_nonzero(changed & ~coverage))
        inside = int(np.count_nonzero(changed & coverage))
        assert outside == 0
        assert inside > 0
        return {
            "changed_pixels": int(np.count_nonzero(changed)),
            "changed_inside_mask_rays": inside,
            "changed_outside_mask_rays": outside,
            "mask_coverage_pixels": int(np.count_nonzero(coverage)),
        }

    checks.run(f"{level}:mask_overlay_consistency", mask_consistency)

    def snapshot_reuse_and_rerender() -> dict[str, Any]:
        with tempfile.TemporaryDirectory() as directory:
            temp = Path(directory)
            temp_store = BufferStore(temp / "buffers")
            _copy_buffer_tree(buffer_store, temp_store, params.parameter_hash)
            manager = SnapshotManager(temp_store)
            target = temp / "snapshot.json"
            reused = manager.restore(snapshot, params, volume, target)
            assert reused.reused_buffer and not reused.rerendered
            changed = params.with_changes(step_size=params.step_size * 1.08)
            rerendered = manager.restore(
                snapshot,
                changed,
                volume,
                temp / "rerendered.json",
                save_after_rerender=False,
            )
            assert rerendered.rerendered and not rerendered.reused_buffer
            assert "step_size" in rerendered.changed_fields
            assert rerendered.result.parameter_hash == changed.parameter_hash
            return {
                "reused_buffer_hash": reused.result.buffer_hash,
                "rerendered_buffer_hash": rerendered.result.buffer_hash,
                "changed_fields": list(rerendered.changed_fields),
            }

    checks.run(f"{level}:snapshot_reuse_and_rerender", snapshot_reuse_and_rerender)

    def snapshot_invalid_and_no_writes() -> dict[str, Any]:
        with tempfile.TemporaryDirectory() as directory:
            temp = Path(directory)
            corrupted = temp / "corrupt.json"
            payload = json.loads(
                (root / "snapshots" / str(record["baseline_snapshot"])).read_text(encoding="utf-8")
            )
            del payload["version"]
            corrupted.write_text(json.dumps(payload), encoding="utf-8")
            before = sorted(path.name for path in temp.iterdir())
            error = _expect_error(ErrorCode.E_SNAPSHOT_INVALID, lambda: load_snapshot(corrupted))
            after = sorted(path.name for path in temp.iterdir())
            assert before == after
        return {"error": error, "outputs_written": False}

    checks.run(f"{level}:snapshot_invalid_no_output", snapshot_invalid_and_no_writes)

    def crash_recovery_from_disk() -> dict[str, Any]:
        outcome = recover_from_snapshot(
            root / "snapshots" / str(record["baseline_snapshot"]),
            volume,
            buffer_store,
        )
        assert outcome.result.parameter_hash == params.parameter_hash
        assert outcome.reused_buffer is True
        return {
            "parameter_hash": outcome.result.parameter_hash,
            "buffer_hash": outcome.result.buffer_hash,
            "source": "disk_snapshot_only",
        }

    checks.run(f"{level}:crash_recovery_from_snapshot", crash_recovery_from_disk)

    def header_error() -> dict[str, Any]:
        with tempfile.TemporaryDirectory() as directory:
            corrupt = Path(directory) / "bad-header.vol"
            payload = bytearray((root / "samples" / str(record["path"])).read_bytes())
            struct.pack_into("<Q", payload, 56, int(volume.header.density_bytes) + 2)
            corrupt.write_bytes(payload)
            return _expect_error(ErrorCode.E_HEADER_INVALID, lambda: load_volume(corrupt))

    checks.run(f"{level}:header_invalid_code", header_error)

    def mask_error() -> dict[str, Any]:
        with tempfile.TemporaryDirectory() as directory:
            corrupt = Path(directory) / "bad-mask.vol"
            payload = bytearray((root / "samples" / str(record["path"])).read_bytes())
            struct.pack_into("<I", payload, 80, max(1, volume.dims[0] - 1))
            corrupt.write_bytes(payload)
            return _expect_error(ErrorCode.E_MASK_INVALID, lambda: load_volume(corrupt))

    checks.run(f"{level}:mask_invalid_code", mask_error)

    def trace_fields() -> dict[str, Any]:
        records = read_trace(root / "traces" / str(record["baseline_trace"]))
        render_records = [item for item in records if item.get("event") == "baseline_render"]
        assert len(render_records) == 1
        item = render_records[0]
        for field in ("parameter_hash", "buffer_hash", "view_matrix", "step_size", "mask_enabled"):
            assert field in item
        assert item["parameter_hash"] == params.parameter_hash
        assert item["buffer_hash"] == baseline_result.buffer_hash
        return {
            "trace": record["baseline_trace"],
            "records": len(records),
            "render_fields": sorted(item),
        }

    checks.run(f"{level}:trace_required_fields", trace_fields)


def _service_checks(checks: _Checks, root: Path) -> None:
    volume = load_volume(root / "samples" / "synthetic_64x64x64.vol")
    params = baseline_parameters(volume, viewport=(32, 32)).with_changes(step_size=0.08)
    release = threading.Event()
    entered = threading.Event()

    def controlled(volume_arg, params_arg, cancel_token=None, batch_size=8):
        entered.set()
        while not release.wait(0.003):
            if cancel_token is not None and cancel_token.is_set():
                raise RenderCancelled("cancelled by verification")
        return render_offscreen(volume_arg, params_arg, batch_size=batch_size)

    def busy_check() -> dict[str, Any]:
        service = OffscreenRenderService(renderer=controlled, max_workers=1)
        try:
            first = service.submit("verification-volume", volume, params)
            assert entered.wait(2.0)
            error = _expect_error(
                ErrorCode.E_BUSY,
                lambda: service.submit("verification-volume", volume, params),
            )
            release.set()
            result = first.future.result(timeout=10.0)
            assert result.parameter_hash == params.parameter_hash
            return {"error": error, "completed_parameter_hash": result.parameter_hash}
        finally:
            release.set()
            service.shutdown(wait=True)

    checks.run("concurrency:duplicate_is_busy", busy_check)

    def cancel_check() -> dict[str, Any]:
        release.clear()
        entered.clear()
        service = OffscreenRenderService(renderer=controlled, max_workers=1)
        with tempfile.TemporaryDirectory() as directory:
            sentinel = Path(directory) / "last-committed.json"
            sentinel.write_bytes(b"keep-me")
            try:
                job = service.submit(
                    "verification-volume",
                    volume,
                    params,
                    snapshot_path=sentinel,
                )
                assert entered.wait(2.0)
                assert service.cancel("verification-volume")
                try:
                    job.future.result(timeout=10.0)
                except RenderCancelled:
                    pass
                else:
                    raise AssertionError("cancelled task unexpectedly completed")
                deadline = time.time() + 2.0
                while service.active_volume_keys() and time.time() < deadline:
                    time.sleep(0.01)
                assert service.active_volume_keys() == ()
                assert sentinel.read_bytes() == b"keep-me"
                return {
                    "cancelled": True,
                    "active_after_cancel": list(service.active_volume_keys()),
                    "last_snapshot_preserved": True,
                }
            finally:
                release.set()
                service.shutdown(wait=True)

    checks.run("concurrency:cooperative_cancel", cancel_check)


def _determinism_check(checks: _Checks) -> None:
    def deterministic_generation_and_trace() -> dict[str, Any]:
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            left_samples = base / "left" / "samples"
            right_samples = base / "right" / "samples"
            left_generate = generate_samples(left_samples, sizes=(64,), seed=DEFAULT_SEED)
            right_generate = generate_samples(right_samples, sizes=(64,), seed=DEFAULT_SEED)
            left_file = left_samples / str(left_generate[0]["path"])
            right_file = right_samples / str(right_generate[0]["path"])
            assert left_file.read_bytes() == right_file.read_bytes()
            left_volume = load_volume(left_file)
            right_volume = load_volume(right_file)
            assert left_volume.content_sha256 == right_volume.content_sha256
            left_params = baseline_parameters(left_volume, viewport=(48, 48)).with_changes(step_size=0.04)
            right_params = baseline_parameters(right_volume, viewport=(48, 48)).with_changes(step_size=0.04)
            assert left_params.parameter_hash == right_params.parameter_hash
            left_result = render_offscreen(left_volume, left_params)
            right_result = render_offscreen(right_volume, right_params)
            assert left_result.rgba_bytes == right_result.rgba_bytes
            left_trace = base / "left" / "trace.jsonl"
            right_trace = base / "right" / "trace.jsonl"
            for trace_path, volume, params, result in (
                (left_trace, left_volume, left_params, left_result),
                (right_trace, right_volume, right_params, right_result),
            ):
                with DeterministicTrace(trace_path) as trace:
                    trace.record(
                        "generated",
                        seed=DEFAULT_SEED,
                        content_sha256=volume.content_sha256,
                        density_sha256=volume.density_sha256,
                        mask_sha256=volume.mask_sha256,
                    )
                    trace.record_render("render", params, result)
            assert left_trace.read_bytes() == right_trace.read_bytes()
            return {
                "volume_byte_identical": True,
                "parameter_hash": left_params.parameter_hash,
                "buffer_hash": left_result.buffer_hash,
                "trace_byte_identical": True,
            }

    checks.run("determinism:generation_render_trace", deterministic_generation_and_trace)


def run_acceptance(root: str | Path) -> dict[str, Any]:
    root_path = Path(root)
    manifest_path = root_path / "samples" / "samples.json"
    if not manifest_path.is_file():
        from .pipeline import generate_project_assets

        generate_project_assets(root_path)
    records, buffer_store = _load_levels(root_path)
    checks = _Checks()
    for level in sorted(records, key=int):
        _level_checks(checks, root_path, records[level], buffer_store)
    _service_checks(checks, root_path)
    _determinism_check(checks)
    passed = all(record["passed"] for record in checks.records)
    report = {
        "schema": "volrender-acceptance-v1",
        "version": 1,
        "passed": passed,
        "check_count": len(checks.records),
        "failed_count": sum(1 for record in checks.records if not record["passed"]),
        "checks": checks.records,
    }
    reports = root_path / "reports"
    reports.mkdir(parents=True, exist_ok=True)
    (reports / "verification.json").write_text(
        json.dumps(report, ensure_ascii=True, sort_keys=True, indent=2) + "\n",
        encoding="utf-8",
    )
    _write_markdown_report(reports / "verification.md", report)
    return report


def _write_markdown_report(path: Path, report: dict[str, Any]) -> None:
    lines = [
        "# Verification Report",
        "",
        f"- Overall: {'PASS' if report['passed'] else 'FAIL'}",
        f"- Checks: {report['check_count']}",
        f"- Failures: {report['failed_count']}",
        "",
        "| Check | Result |",
        "| --- | --- |",
    ]
    for record in report["checks"]:
        result = "PASS" if record["passed"] else "FAIL"
        lines.append(f"| `{record['name']}` | {result} |")
    lines.append("")
    path.write_text("\n".join(lines), encoding="utf-8")
