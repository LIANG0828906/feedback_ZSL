"""需求 7: 实时 / 断点恢复 / 全量重算三路径一致(E_DIVERGE)。"""

import pytest

from a11y_media import compare
from a11y_media.checkpoint import CheckpointStore
from a11y_media.engine import Engine, ProjectResult, _Crash, run_full
from a11y_media.errors import E_DIVERGE, ComponentError


def _three_paths(project, tmp_path):
    rt = Engine(project, store=CheckpointStore(str(tmp_path / "rt"))).run()
    store = CheckpointStore(str(tmp_path / "rs"))
    crash_at = max(1, project.num_chunks // 2)
    if crash_at < project.num_chunks:
        with pytest.raises(_Crash):
            Engine(project, store=store).run(crash_after=crash_at)
    resumed = Engine(project, store=store).run()
    full = run_full(project)
    return {"realtime": rt, "resume": resumed, "full": full}


def test_three_paths_consistent(projects, tmp_path):
    for i, p in enumerate(projects):
        results = _three_paths(p, tmp_path / f"p{i}")
        compare.assert_consistent(results)  # 不抛异常即一致


def test_diverge_reports_first_mismatch(project):
    a = run_full(project)
    bad_windows = [list(w) for w in a.windows]
    bad_windows[1] = [bad_windows[1][0] + 5, bad_windows[1][1]]
    b = ProjectResult(bad_windows, a.char_starts, a.report, a.progress)
    div = compare.first_divergence(a, b)
    assert div["sub"] == 1 and div["field"] == "window"
    assert div["path_a"] != div["path_b"]
    with pytest.raises(ComponentError) as ei:
        compare.assert_consistent({"realtime": a, "full": b})
    assert ei.value.code == E_DIVERGE
    assert ei.value.context["sub"] == 1


def test_diverge_on_char_start(project):
    a = run_full(project)
    bad_starts = [list(s) for s in a.char_starts]
    bad_starts[0][2] += 1
    b = ProjectResult(a.windows, bad_starts, a.report, a.progress)
    div = compare.first_divergence(a, b)
    assert div["sub"] == 0 and div["char"] == 2 and div["field"] == "char_start"
