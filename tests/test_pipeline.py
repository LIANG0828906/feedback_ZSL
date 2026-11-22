import json
import os

import pytest

from subpipeline.errors import E_GAP, E_PARA, PipelineError
from subpipeline.loader import load_project
from subpipeline.pipeline import Pipeline
from subpipeline.validator import verify_run


def _load(samples_root, name):
    return load_project(os.path.join(samples_root, name))


def test_parallel_matches_serial_and_truth(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    serial = Pipeline(project, manifest, wave, runs_root=runs_root,
                      max_workers=1)
    s_results, s_failed = serial.run(parallel=False)

    parallel = Pipeline(project, manifest, wave, runs_root=runs_root,
                        max_workers=4, max_buffers=4)
    p_results, p_failed = parallel.run(parallel=True)

    assert not s_failed and not p_failed
    assert [r.to_dict() for r in s_results] == [r.to_dict() for r in p_results]
    assert [r.subtitle_id for r in p_results] == \
        [s.subtitle_id for s in project.subtitles]

    truth = json.load(open(os.path.join(
        samples_root, "proj_main_20261123", "ground_truth.json")))
    violation = verify_run(
        os.path.join(samples_root, "proj_main_20261123"),
        parallel.run_dir, tolerance_s=0.02, truth=truth)
    assert violation is None


def test_narrow_project_three_way_consistency(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_narrow_20261124")
    runs = []
    for workers in (1, 2, 4):
        pipe = Pipeline(project, manifest, wave, runs_root=runs_root,
                        max_workers=workers)
        results, failed = pipe.run(parallel=workers > 1)
        assert not failed
        runs.append([r.to_dict() for r in results])
    assert runs[0] == runs[1] == runs[2]


def test_tight_project_failure_isolated(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_tight_20261125")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root,
                    max_workers=3)
    results, failed = pipe.run(parallel=True)
    assert failed, "expected at least one E_GAP failure"
    codes = {exc.code for exc in failed.values()}
    assert E_GAP in codes
    # Other subtitles still committed; a failed subtitle never blocks them.
    assert 0 < len(results) < len(project.subtitles)
    states = pipe.board.snapshot()
    assert any(state == "failed" for state in states.values())
    assert any(state == "committed" for state in states.values())


def test_tiny_project_epara(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_tiny_20261126")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root)
    _results, failed = pipe.run(parallel=False)
    assert all(exc.code == E_PARA for exc in failed.values())


def test_checkpoint_resume_reuses_committed(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root,
                    max_workers=2)
    results, _ = pipe.run(parallel=True)
    run_dir = pipe.run_dir

    # New pipeline instance over the same run directory recovers state.
    resumed = Pipeline(project, manifest, wave, runs_root=runs_root,
                       max_workers=2, run_dir=run_dir)
    resumed_results, _ = resumed.run(parallel=True)
    assert [r.to_dict() for r in resumed_results] == \
        [r.to_dict() for r in results]
    records = os.listdir(os.path.join(run_dir, "records"))
    assert len(records) == len(project.subtitles)


def test_snapshot_invalidated_on_param_change(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root)
    pipe.run(parallel=False)
    run_dir = pipe.run_dir

    changed_manifest = json.loads(json.dumps(manifest))
    changed_manifest["params"]["max_chars_per_line"] = 8
    pipe2 = Pipeline(project, changed_manifest, wave, runs_root=runs_root,
                     run_dir=run_dir)
    results, meta = pipe2.store.load()
    assert meta == "invalidated"
    pipe2.run(parallel=False)
    # After invalidation the snapshot was rebuilt from scratch.
    fresh, meta2 = pipe2.store.load()
    assert meta2 != "invalidated"
    assert len(fresh) == len(project.subtitles)


def test_forced_stop_does_not_emit_full_timeline(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root,
                    max_workers=2)
    import threading
    thread = threading.Thread(target=pipe.run, kwargs={"parallel": True})
    thread.start()
    # Stop immediately; a subtitle or two may finish, never the full timeline.
    pipe.stop()
    thread.join(timeout=5)
    recovered, _meta = pipe.store.load()
    assert len(recovered) < len(project.subtitles)


def test_validator_flags_first_violation(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root)
    pipe.run(parallel=False)
    # Corrupt one persisted record's window.
    record_files = sorted(os.listdir(os.path.join(pipe.run_dir, "records")))
    target = os.path.join(pipe.run_dir, "records", record_files[1])
    record = json.load(open(target))
    record["result"]["end_sample"] += 5000
    json.dump(record, open(target, "w"))
    truth = json.load(open(os.path.join(
        samples_root, "proj_main_20261123", "ground_truth.json")))
    violation = verify_run(
        os.path.join(samples_root, "proj_main_20261123"),
        pipe.run_dir, truth=truth)
    assert violation is not None
    assert violation.subtitle_id == record["result"]["subtitle_id"]


def test_validator_decode_error_on_garbage(samples_root, runs_root):
    project, manifest, wave, _h = _load(samples_root, "proj_main_20261123")
    pipe = Pipeline(project, manifest, wave, runs_root=runs_root)
    pipe.run(parallel=False)
    bad = os.path.join(pipe.run_dir, "records", "garbage.json")
    open(bad, "w").write("{broken")
    from subpipeline.errors import E_DECODE
    with pytest.raises(PipelineError) as exc:
        verify_run(os.path.join(samples_root, "proj_main_20261123"),
                   pipe.run_dir)
    assert exc.value.code == E_DECODE
