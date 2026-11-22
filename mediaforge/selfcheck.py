from __future__ import annotations

import importlib.util
import json
import sys
import tempfile
from pathlib import Path
from typing import Any

from .errors import E_CORRUPT, E_IO, E_META, WorkbenchError, fail
from .injection import InjectionConfig
from .jsonutil import sha256_bytes
from .package import build_package, write_package
from .pipeline import discover_sample_groups, load_sample, metadata_path, truth_path
from .segmented import pack_segmented
from .truth import compute_truth_fingerprint, load_truth
from .validator import validate_artifact


def _read_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except OSError as exc:
        fail(E_IO, "$.samples", f"cannot read {path}: {exc}")
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(E_CORRUPT, "$.samples", f"invalid JSON in {path}: {exc}")


def run_selfcheck(
    samples_root: str | Path = "samples",
    *,
    checkpoint_root: str | Path | None = None,
) -> dict[str, Any]:
    root = Path(samples_root)
    checks: list[dict[str, Any]] = []
    failures: list[dict[str, Any]] = []

    def record(name: str, ok: bool, detail: Any) -> None:
        checks.append({"check": name, "ok": ok, "detail": detail})
        if not ok:
            failures.append({"check": name, "detail": detail})

    index_path = root / "index.json"
    if not index_path.exists():
        fail(E_IO, "$.index", "samples/index.json is missing; run the sample generator first")
    index = _read_json(index_path)
    groups = discover_sample_groups(root)
    record("sample_group_count", len(groups) >= 3, {"count": len(groups), "required": 3})
    record("metadata_version_count", index.get("metadata_versions") == ["v1", "v2", "v3"], index.get("metadata_versions"))

    index_by_group = {item["sample_id"]: item for item in index.get("groups", [])}
    for group in groups:
        truth_file = truth_path(group)
        truth_value = _read_json(truth_file)
        measured_fingerprint = compute_truth_fingerprint(truth_value)
        declared_fingerprint = truth_value.get("fingerprint")
        record(
            f"{group.name}.truth_fingerprint",
            measured_fingerprint == declared_fingerprint,
            {"measured": measured_fingerprint, "declared": declared_fingerprint},
        )
        index_entry = index_by_group.get(group.name)
        record(
            f"{group.name}.index_fingerprint",
            bool(index_entry) and index_entry.get("truth_fingerprint") == declared_fingerprint,
            index_entry.get("truth_fingerprint") if index_entry else None,
        )
        for relative, declared_hash in sorted(truth_value.get("files", {}).items()):
            path = group / relative
            record(
                f"{group.name}.file_hash.{relative}",
                path.exists() and sha256_bytes(path.read_bytes()) == declared_hash,
                {"path": str(path), "declared": declared_hash},
            )
        loaded_counts: dict[str, int] = {}
        for version in ("v1", "v2", "v3"):
            loaded = load_sample(group, version)
            loaded_counts[version] = len(loaded.model.tracks)
            truth = loaded.truth
            assert truth is not None
            track_ok = all(
                track.sample_count == next(item for item in truth.tracks if item.track_id == track.track_id).sample_count
                for track in loaded.model.tracks
            )
            record(
                f"{group.name}.{version}.import",
                track_ok
                and loaded.model.duration_ticks == truth.duration_ticks
                and tuple(chapter.chapter_id for chapter in loaded.model.chapters)
                == tuple(chapter.chapter_id for chapter in truth.chapters),
                {
                    "source_version": loaded.model.source_version,
                    "tracks": len(loaded.model.tracks),
                    "duration_ticks": loaded.model.duration_ticks,
                },
            )
        record(f"{group.name}.all_versions_same_track_count", len(set(loaded_counts.values())) == 1, loaded_counts)

    first_group = groups[0]
    sample = load_sample(first_group, "v3")
    with tempfile.TemporaryDirectory(prefix="mediaforge-selfcheck-") as temp:
        artifact = Path(temp) / "selfcheck.mfz"
        package = build_package(sample.model, "v3")
        write_package(package, artifact)
        validation = validate_artifact(artifact, truth_path(first_group))
        record(
            "independent_validator",
            validation.ok,
            validation.to_dict(),
        )

    final_checkpoint = Path(checkpoint_root) if checkpoint_root else Path("artifacts") / "selfcheck-checkpoints"
    segmented = pack_segmented(
        sample.model,
        "v3",
        final_checkpoint,
        workers=2,
        parameter_version="selfcheck-p1",
        truth_path=truth_path(first_group),
    )
    record(
        "checkpoint_pack",
        segmented.ok and segmented.validation is not None and segmented.validation.ok,
        segmented.to_dict(),
    )
    probe = Path(final_checkpoint) / ".selfcheck-write-probe"
    probe.parent.mkdir(parents=True, exist_ok=True)
    probe.write_bytes(b"mediaforge-selfcheck")
    probe_ok = probe.read_bytes() == b"mediaforge-selfcheck"
    probe.unlink()
    record(
        "checkpoint_writable_and_cleanable",
        probe_ok and not probe.exists(),
        str(final_checkpoint),
    )

    injection = InjectionConfig(corrupt_track="audio_main", corrupt_offset=17, parameter_version="selfcheck-p1")
    corrupted = pack_segmented(
        sample.model,
        "v3",
        final_checkpoint,
        workers=2,
        parameter_version="selfcheck-p1",
        injection=injection,
    )
    record(
        "injected_corruption",
        not corrupted.ok
        and len(corrupted.failures) == 1
        and corrupted.failures[0].code == "E_CORRUPT",
        [failure.to_dict() for failure in corrupted.failures],
    )
    recovered = pack_segmented(
        sample.model,
        "v3",
        final_checkpoint,
        workers=2,
        parameter_version="selfcheck-p1",
    )
    record(
        "checkpoint_retry",
        recovered.ok and "audio_main" in recovered.built_tracks and "video_main" in recovered.reused_tracks,
        {
            "built": recovered.built_tracks,
            "reused": recovered.reused_tracks,
            "artifact_sha256": recovered.artifact_sha256,
        },
    )

    dependencies = {
        "numpy": importlib.util.find_spec("numpy") is not None,
        "PySide6": importlib.util.find_spec("PySide6") is not None,
        "PyQt5_compat": importlib.util.find_spec("PyQt5") is not None,
        "python": f"{sys.version_info.major}.{sys.version_info.minor}",
    }
    return {
        "ok": not failures,
        "samples_root": str(root.resolve()),
        "check_count": len(checks),
        "failure_count": len(failures),
        "failures": failures,
        "checks": checks,
        "dependencies": dependencies,
    }
