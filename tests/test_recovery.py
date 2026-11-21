from __future__ import annotations

import json
import os
import subprocess
import sys
from dataclasses import replace
from pathlib import Path

import pytest

from npr_pipeline.errors import E_RECOVERY_INVALID, PipelineError
from npr_pipeline.pipeline import RenderPipeline
from npr_pipeline.recovery import recover
from npr_pipeline.storage import Store


def _cli_env() -> dict[str, str]:
    env = os.environ.copy()
    env["PYTHONPATH"] = str(Path(__file__).resolve().parents[1] / "src")
    return env


def test_forced_termination_then_resume(
    sample_root,
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    first = RenderPipeline(store).render(balanced_scene, balanced_params)
    changed = replace(
        balanced_params,
        outline=replace(
            balanced_params.outline,
            width=balanced_params.outline.width + 0.25,
        ),
    )
    params_path = tmp_path / "params.json"
    params_path.write_text(json.dumps(changed.to_dict()), encoding="utf-8")
    scene_path = sample_root / "balanced" / "scene.json"
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "npr_pipeline.cli",
            "render",
            "--scene",
            str(scene_path),
            "--params",
            str(params_path),
            "--workdir",
            str(store.root),
            "--crash-after-batch",
            "2",
        ],
        env=_cli_env(),
        capture_output=True,
        text=True,
        check=False,
    )
    assert completed.returncode == 91
    current = store.current_manifest()
    assert current is not None and current["version"] == first.version
    recovery = recover(store, cleanup=True, keep_staging=True)
    assert recovery.resume_available
    assert len(recovery.completed_batches) == 2
    assert recovery.missing_batches
    resumed = RenderPipeline(store).render(balanced_scene, changed, resume=True)
    assert resumed.version == first.version + 1
    assert resumed.resumed_batches >= 2
    assert Store(store.root).current_manifest()["version"] == resumed.version


def test_tampered_chunk_is_recovery_invalid(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    result = RenderPipeline(store).render(balanced_scene, balanced_params)
    record = result.manifest["batches"][0]
    chunk_path = store.root / record["file"]
    chunk_path.write_bytes(chunk_path.read_bytes() + b"corrupt")
    with pytest.raises(PipelineError) as caught:
        recover(store)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.location == f"batch:{record['batch']}"
    assert caught.value.detail == "chunk_hash_mismatch"


def test_manifest_version_mismatch_locates_manifest(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    RenderPipeline(store).render(balanced_scene, balanced_params)
    pointer = json.loads(store.current_path.read_text(encoding="utf-8"))
    pointer["version"] = 9
    store.current_path.write_text(json.dumps(pointer), encoding="utf-8")
    with pytest.raises(PipelineError) as caught:
        recover(store)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.location == "CURRENT"
    assert caught.value.detail in {
        "manifest_path_version_mismatch",
        "missing_file",
    }


def test_missing_previous_manifest_breaks_hash_chain(
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    pipeline = RenderPipeline(store)
    pipeline.render(balanced_scene, balanced_params)
    changed = replace(
        balanced_params,
        smear=replace(
            balanced_params.smear,
            width=balanced_params.smear.width + 0.3,
        ),
    )
    pipeline.render(balanced_scene, changed)
    (store.manifests_dir / "v000001.json").unlink()
    with pytest.raises(PipelineError) as caught:
        recover(store)
    assert caught.value.code == E_RECOVERY_INVALID
    assert caught.value.location == "manifests/v000001.json"
    assert caught.value.detail == "missing_file"


def test_discarding_staging_removes_uncommitted_chunks(
    sample_root,
    balanced_scene,
    balanced_params,
    tmp_path,
) -> None:
    store = Store(tmp_path / "work")
    RenderPipeline(store).render(balanced_scene, balanced_params)
    changed = replace(
        balanced_params,
        shadow=replace(
            balanced_params.shadow,
            spacing=balanced_params.shadow.spacing + 0.9,
        ),
    )
    params_path = tmp_path / "params.json"
    params_path.write_text(json.dumps(changed.to_dict()), encoding="utf-8")
    subprocess.run(
        [
            sys.executable,
            "-m",
            "npr_pipeline.cli",
            "render",
            "--scene",
            str(sample_root / "balanced" / "scene.json"),
            "--params",
            str(params_path),
            "--workdir",
            str(store.root),
            "--crash-after-batch",
            "2",
        ],
        env=_cli_env(),
        capture_output=True,
        text=True,
        check=False,
    )
    before = set(store.chunks_dir.rglob("*.nprb"))
    recovery = recover(store, cleanup=True, keep_staging=False)
    after = set(store.chunks_dir.rglob("*.nprb"))
    assert recovery.discarded_staging
    assert len(before) > len(after)
    assert recovery.discarded_chunks
    assert not (store.staging_dir / "state.json").exists()

