import pytest

from scoreforge.checkpoint import CheckpointStore
from scoreforge.errors import DivergenceError
from scoreforge.generator import generate_suite
from scoreforge.recompute import (
    compare_scores,
    full_recompute,
    make_edit_script,
    realtime_run,
    resume_run,
)


@pytest.mark.parametrize("index", [0, 1, 2])
def test_three_paths_agree(index, tmp_path):
    data = generate_suite(42, 3)[index]
    script = make_edit_script(data)
    a = realtime_run(data, script)
    c = full_recompute(data, script)
    store = CheckpointStore(tmp_path, data["project_id"])
    b = resume_run(data, script, store, crash_after_commits=3)
    compare_scores(a, b)
    compare_scores(a, c)
    compare_scores(b, c)


def test_divergence_reports_path_and_field(project_data):
    script = make_edit_script(project_data)
    a = realtime_run(project_data, script)
    bad = full_recompute(project_data, script)
    pid = bad.part_ids[0]
    note = bad.part(pid).measures[1].notes[0]
    note.duration_ticks += 120
    with pytest.raises(DivergenceError) as exc:
        compare_scores(a, bad)
    assert exc.value.code == "E_DIVERGE"
    assert exc.value.field == "duration_ticks"
    assert note.note_id in exc.value.path
    assert exc.value.left != exc.value.right
