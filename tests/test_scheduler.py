"""要求 6：受限预算并行与串行提交一致性。"""
import numpy as np


def test_parallel_serial_pixel_identical(pipe):
    meshes = pipe.tessellate_all(pipe.cfg.precise_density)
    version = pipe.snapshot.version
    chash = pipe.snapshot.content_hash

    f_par, s_par = pipe.render_precise(parallel=True)
    pipe.store.discard()
    pipe2 = type(pipe)(pipe.cfg)
    f_ser, s_ser = pipe2.render_precise(parallel=False)

    assert s_par.order == s_ser.order
    np.testing.assert_array_equal(f_par.color, f_ser.color)
    assert f_par.tile_grid == f_ser.tile_grid


def test_commit_order_stable_and_all_committed(pipe):
    _, result = pipe.render_precise(parallel=True)
    n = len(pipe.scheduler.build_jobs())
    assert result.order == list(range(n))
    assert all(st.state == "committed"
               for st in result.statuses.values())


def test_budget_queue_records_events(pipe):
    # 极紧像素预算，迫使获取预算时排队
    pipe.scheduler.max_inflight_pixels = pipe.cfg.tile_size ** 2
    _, result = pipe.render_precise(parallel=True)
    reasons = {e["reason"] for e in result.queue_events}
    assert "pixel_buffer_budget" in reasons
    # 排队不丢块
    assert len(result.order) == len(pipe.scheduler.build_jobs())


def test_tile_failure_isolated_and_retried(pipe, monkeypatch):
    calls = {"n": 0}
    original = pipe.rast.render_tile

    def flaky(meshes, x0, y0, w, h, mode, version):
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("inject tile failure")
        return original(meshes, x0, y0, w, h, mode, version)

    monkeypatch.setattr(pipe.rast, "render_tile", flaky)
    _, result = pipe.render_precise(parallel=True)
    # 首次失败后重试成功，其他块不受污染
    assert len(result.failed) == 0
    assert any(st.attempts == 2 for st in result.statuses.values())


def test_latency_quantiles_present(pipe):
    _, result = pipe.render_precise(parallel=True)
    q = result.quantiles()
    assert set(q) == {"p50", "p90", "p99"}
