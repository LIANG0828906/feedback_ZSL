import threading
import time

from scoreforge.concurrency import ParallelCommitter, commit_sequence
from scoreforge.model import Score
from scoreforge.recompute import apply_op, compare_scores, make_edit_script
from scoreforge.session import EditSession


def _staged_session(project_data):
    session = EditSession(Score(project_data))
    for op in make_edit_script(project_data):
        if op["op"] != "commit":
            apply_op(session, op)
    return session


def test_parallel_matches_serial(project_data):
    par = _staged_session(project_data)
    results_par = ParallelCommitter(par, max_workers=4, buffer_size=2) \
        .commit_all_pending()
    seq = _staged_session(project_data)
    results_seq = ParallelCommitter(seq, max_workers=1, buffer_size=1) \
        .commit_all_pending()
    assert commit_sequence(results_par) == commit_sequence(results_seq)
    compare_scores(par.score, seq.score)
    assert [r.record.seq for r in results_par if r.record] \
        == list(range(len(results_par)))


def test_buffer_limit_blocks_without_dropping(project_data):
    session = _staged_session(project_data)
    committer = ParallelCommitter(session, max_workers=2, buffer_size=1)
    keys = session.pending_keys()
    results = committer.commit(keys)
    assert len(results) == len(keys)  # 全部处理，无静默丢弃
    assert all(r.status == "committed" for r in results)


def test_failure_isolation_and_retry(project_data):
    session = _staged_session(project_data)
    pid = session.score.part_ids[0]
    bad_note = session.working.part(pid).measures[1].notes[0]
    good = bad_note.duration_ticks
    bad_note.duration_ticks = -5
    results = ParallelCommitter(session, max_workers=4, buffer_size=4) \
        .commit_all_pending()
    failed = [r for r in results if r.status == "failed"]
    assert len(failed) == 1
    assert failed[0].error.code == "E_DUR"
    assert (pid, 1) not in session.committed
    committed_elsewhere = [
        r for r in results
        if r.status == "committed" and (r.part_id, r.measure_index) != (pid, 1)
    ]
    assert committed_elsewhere  # 其他小节未被污染
    session.set_note_duration(pid, 1, bad_note.note_id, good)
    retry = ParallelCommitter(session, max_workers=2, buffer_size=2).commit([(pid, 1)])
    assert retry[0].status == "committed"


def test_semaphore_bounds_inflight(project_data):
    session = _staged_session(project_data)
    inflight = 0
    peak = 0
    lock = threading.Lock()
    original_prepare = session.prepare_commit

    def counting_prepare(*args):
        nonlocal inflight, peak
        with lock:
            inflight += 1
            peak = max(peak, inflight)
        try:
            time.sleep(0.001)
            return original_prepare(*args)
        finally:
            with lock:
                inflight -= 1

    session.prepare_commit = counting_prepare
    ParallelCommitter(session, max_workers=8, buffer_size=3).commit_all_pending()
    assert peak <= 3  # 缓冲上限约束在途任务数
