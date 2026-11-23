"""要求 7：检查点原子写入、哈希校验与崩溃恢复边界。"""
import json
import os

import numpy as np


def test_commit_tile_persisted(pipe):
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    job = pipe.scheduler.build_jobs()[0]
    c, d = pipe.rast.render_tile(meshes, job.x0, job.y0, job.w, job.h,
                                 "precise", pipe.snapshot.version)
    rec = pipe.store.commit_tile(0, job.tx, job.ty,
                                 pipe.snapshot.version,
                                 pipe.snapshot.content_hash, c, d)
    assert os.path.exists(os.path.join(pipe.store.tiles_dir, rec.color_file))
    loaded_c, loaded_d = pipe.store.load_tile(rec.to_meta())
    np.testing.assert_array_equal(loaded_c, c)


def test_recover_only_persisted_after_kill(pipe):
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    jobs = pipe.scheduler.build_jobs()
    half = len(jobs) // 2
    for job in jobs[:half]:
        c, d = pipe.rast.render_tile(meshes, job.x0, job.y0, job.w, job.h,
                                     "precise", pipe.snapshot.version)
        pipe.store.commit_tile(job.tile_id, job.tx, job.ty,
                               pipe.snapshot.version,
                               pipe.snapshot.content_hash, c, d)
    # 新管线实例模拟进程重启
    pipe2 = type(pipe)(pipe.cfg)
    recovered = pipe2.store.recover(pipe2.snapshot.version,
                                    pipe2.snapshot.content_hash)
    assert len(recovered) == half

    frame, result = pipe2.render_precise(parallel=True)
    assert getattr(frame, "complete") is True
    assert sorted(result.recovered) == list(range(half))


def test_hash_mismatch_invalidates_all(pipe):
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    job = pipe.scheduler.build_jobs()[0]
    c, d = pipe.rast.render_tile(meshes, job.x0, job.y0, job.w, job.h,
                                 "precise", pipe.snapshot.version)
    pipe.store.commit_tile(0, job.tx, job.ty, pipe.snapshot.version,
                           pipe.snapshot.content_hash, c, d)
    recovered = pipe.store.recover(pipe.snapshot.version,
                                   "wrong_hash_value")
    assert recovered == {}  # 整体作废，需全量重算


def test_algo_version_mismatch_invalidates(pipe, monkeypatch):
    import cadrender.checkpoint as cp
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    job = pipe.scheduler.build_jobs()[0]
    c, d = pipe.rast.render_tile(meshes, job.x0, job.y0, job.w, job.h,
                                 "precise", pipe.snapshot.version)
    pipe.store.commit_tile(0, job.tx, job.ty, pipe.snapshot.version,
                           pipe.snapshot.content_hash, c, d)
    monkeypatch.setattr(cp, "ALGO_VERSION", "cadrender-9.9.9")
    assert pipe.store.recover(pipe.snapshot.version,
                              pipe.snapshot.content_hash) == {}


def test_crash_does_not_claim_complete(pipe):
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    jobs = pipe.scheduler.build_jobs()
    for job in jobs[:3]:
        c, d = pipe.rast.render_tile(meshes, job.x0, job.y0, job.w, job.h,
                                     "precise", pipe.snapshot.version)
        pipe.store.commit_tile(job.tile_id, job.tx, job.ty,
                               pipe.snapshot.version,
                               pipe.snapshot.content_hash, c, d)
    frame = pipe.scheduler.assemble_frame(pipe.snapshot.version,
                                          pipe.snapshot.content_hash,
                                          "precise")
    assert getattr(frame, "complete") is False
