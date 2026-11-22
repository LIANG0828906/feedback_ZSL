"""验收运行器：使用本地确定性样本执行全部验收项并输出报告."""

from __future__ import annotations

import argparse
import copy
import json
import shutil
from pathlib import Path

from .checkpoint import CheckpointStore, restore_session
from .concurrency import ParallelCommitter, commit_sequence
from .errors import ScoreError
from .generator import generate_suite, write_samples
from .model import Score, load_project
from .playback import PlaybackEngine
from .precision import verify_truth
from .recompute import (
    _skip_committed_ops,
    apply_op,
    compare_scores,
    full_recompute,
    make_edit_script,
    realtime_run,
    resume_run,
    run_script,
)
from .session import EditSession
from .units import DEFAULT_SEED


class Reporter:
    def __init__(self):
        self.lines = []

    def log(self, text=""):
        print(text)
        self.lines.append(text)

    def section(self, title):
        self.log()
        self.log(f"===== {title} =====")

    def save(self, path):
        Path(path).write_text("\n".join(self.lines) + "\n", encoding="utf-8")


def _expect_error(rep: Reporter, label: str, fn, expect_code: str) -> bool:
    try:
        fn()
    except ScoreError as exc:
        ok = exc.code == expect_code
        rep.log(f"  [{'OK' if ok else 'FAIL'}] {label}: 捕获 {exc.code} "
                f"(期望 {expect_code}) path={exc.path} {exc.message}")
        return ok
    rep.log(f"  [FAIL] {label}: 未抛出 {expect_code}")
    return False


def run_acceptance(workdir) -> int:
    workdir = Path(workdir)
    if workdir.exists():
        shutil.rmtree(workdir)
    (workdir / "samples").mkdir(parents=True)
    (workdir / "checkpoints").mkdir(parents=True)
    rep = Reporter()
    failures = 0

    def check(ok: bool, label: str):
        nonlocal failures
        rep.log(f"  [{'OK' if ok else 'FAIL'}] {label}")
        if not ok:
            failures += 1

    def expect(label: str, fn, code: str):
        nonlocal failures
        if not _expect_error(rep, label, fn, code):
            failures += 1

    rep.section("1. 样本生成与工程加载校验 (E_SCHEMA / E_REF / E_DUR)")
    paths = write_samples(workdir / "samples", seed=DEFAULT_SEED, count=3)
    projects = [json.loads(p.read_text(encoding="utf-8")) for p in paths]
    rep.log(f"  生成 {len(paths)} 个确定性样本工程 (seed={DEFAULT_SEED})")
    for data in projects:
        score, truth = load_project(data)
        rep.log(f"  {data['project_id']}: 声部={len(score.parts)} "
                f"小节/声部={len(score.measure_structure)} 事件={len(score.events)} "
                f"真值(小节={len(truth['measures'])} 采样={len(truth['dynamics_samples'])} "
                f"时间戳={len(truth['event_timestamps'])})")
    sample = projects[0]

    bad = copy.deepcopy(sample)
    del bad["parts"][0]["measures"][0]["notes"][0]["pitch"]
    expect("字段缺失", lambda: load_project(bad), "E_SCHEMA")

    bad = copy.deepcopy(sample)
    bad["expression_events"][0]["anchor_note_id"] = "NOPE"
    expect("锚点悬空", lambda: load_project(bad), "E_REF")

    bad = copy.deepcopy(sample)
    bad["expression_events"][0]["beat_offset_ticks"] = 10 ** 9
    expect("拍位越过小节", lambda: load_project(bad), "E_REF")

    bad = copy.deepcopy(sample)
    bad["parts"][0]["measures"][0]["notes"][0]["duration_ticks"] = 0
    expect("时值非法", lambda: load_project(bad), "E_DUR")

    rep.section("2. 编辑会话与提交边界 (E_BATCH / E_DANGLING)")
    score, truth = load_project(sample)
    session = EditSession(score)
    pid = score.part_ids[0]
    note = score.part(pid).measures[0].notes[0]
    note1 = score.part(pid).measures[0].notes[1]
    original_duration = note.duration_ticks
    session.set_note_duration(pid, 0, note.note_id, original_duration + 120)
    session.set_note_duration(pid, 0, note1.note_id, note1.duration_ticks - 120)
    check(score.part(pid).measures[0].notes[0].duration_ticks == original_duration,
          "未提交更改不影响内部模型（导出/回放隔离）")
    expect("空批次提交", lambda: session.commit_measure(pid, 1), "E_BATCH")
    record = session.commit_measure(pid, 0)
    rep.log(f"  提交 ({pid}, 0) seq={record.seq} hash={record.content_hash[:16]}…")
    expect("编辑已冻结小节",
           lambda: session.set_note_duration(pid, 0, note.note_id, 240), "E_BATCH")
    expect("重复提交", lambda: session.commit_measure(pid, 0), "E_BATCH")

    anchored = score.events_of_measure(pid, 2)
    if anchored:
        ev = anchored[0]
        session.delete_note(pid, 2, ev.anchor_note_id)
        expect("删除音符残留悬空事件",
               lambda: session.commit_measure(pid, 2), "E_DANGLING")
    else:
        rep.log("  (样本该小节无事件，跳过 E_DANGLING 演示)")

    rep.section("3. 检查点：暂停 / 退出 / 重启续跑 / 版本不匹配作废")
    script = make_edit_script(sample)
    ck_dir = workdir / "checkpoints"
    store = CheckpointStore(ck_dir, sample["project_id"])
    s1 = EditSession(Score(sample), store=store)
    run_script(s1, script, stop_after_commits=4)
    rep.log(f"  模拟强杀前已提交 {len(s1.commit_log)} 个小节")
    del s1
    restored = restore_session(sample, store)
    check(len(restored.committed) == 4, "重启后仅凭检查点恢复 4 个已提交小节")
    run_script(restored, _skip_committed_ops(restored, script))
    a = realtime_run(sample, script)
    try:
        compare_scores(a, restored.score)
        check(True, "断点恢复结果 == 实时编辑结果")
    except ScoreError as exc:
        check(False, f"断点恢复结果 == 实时编辑结果 ({exc})")

    tampered = CheckpointStore(ck_dir, sample["project_id"])
    manifest = json.loads(tampered.manifest_path.read_text(encoding="utf-8"))
    manifest["param_version"] = "0.0-old"
    tampered.manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
    check(tampered.load_validated() is None, "参数版本不匹配 -> 整体快照作废")
    check(not tampered.manifest_path.exists(), "作废后禁止部分混用旧片段（文件已清除）")

    store2 = CheckpointStore(ck_dir, "tamper-hash")
    s2 = EditSession(Score(sample), store=store2)
    run_script(s2, script, stop_after_commits=2)
    frag = sorted(store2.dir.glob("fragment_*.json"))[0]
    payload = json.loads(frag.read_text(encoding="utf-8"))
    payload["fragment"]["measure"]["notes"][0]["duration_ticks"] += 1
    frag.write_text(json.dumps(payload), encoding="utf-8")
    check(store2.load_validated() is None, "内容哈希不匹配 -> 整体快照作废")

    rep.section("4. 三路径一致性 (E_DIVERGE)")
    for data in projects:
        script_n = make_edit_script(data)
        s_a = realtime_run(data, script_n)
        s_c = full_recompute(data, script_n)
        store_n = CheckpointStore(ck_dir, data["project_id"] + "-consistency")
        s_b = resume_run(data, script_n, store_n, crash_after_commits=3)
        try:
            compare_scores(s_a, s_b)
            compare_scores(s_a, s_c)
            check(True, f"{data['project_id']}: 实时 == 恢复 == 重算")
        except ScoreError as exc:
            check(False, f"{data['project_id']}: {exc}")

    s_bad = full_recompute(sample, make_edit_script(sample))
    s_bad.part(pid).measures[1].notes[0].duration_ticks += 120
    expect("篡改一侧结果触发比对",
           lambda: compare_scores(a, s_bad), "E_DIVERGE")

    rep.section("5. 真值精度校验 (E_PREC)")
    for data in projects:
        score_n, truth_n = load_project(data)
        report = verify_truth(score_n, truth_n)
        check(report.passed, f"{data['project_id']}: 精度校验通过 "
                             f"({report.summary().splitlines()[0]})")
        rep.log(f"    真值缺失小节显式标记不可校验: {len(report.unverifiable)} 个")
    edited = full_recompute(sample, make_edit_script(sample))
    edited.part(pid).measures[0].notes[0].duration_ticks += 240
    report = verify_truth(edited, sample["truth"])
    check(not report.passed, "时值被篡改后精度校验失败")
    expect("超容差抛出", lambda: report.raise_if_failed(), "E_PREC")

    rep.section("6. 并发与资源边界")
    script_p = make_edit_script(sample)
    sess_par = EditSession(Score(sample))
    for op in script_p:
        if op["op"] != "commit":
            apply_op(sess_par, op)
    results_par = ParallelCommitter(sess_par, max_workers=4, buffer_size=2) \
        .commit_all_pending()
    sess_seq = EditSession(Score(sample))
    for op in script_p:
        if op["op"] != "commit":
            apply_op(sess_seq, op)
    results_seq = ParallelCommitter(sess_seq, max_workers=1, buffer_size=1) \
        .commit_all_pending()
    check(commit_sequence(results_par) == commit_sequence(results_seq),
          "并行(4线程/缓冲2) 与 串行(1线程/缓冲1) 提交序列一致")
    try:
        compare_scores(sess_par.score, sess_seq.score)
        check(True, "并行与串行最终模型一致")
    except ScoreError as exc:
        check(False, f"并行与串行最终模型一致 ({exc})")

    sess_fail = EditSession(Score(sample))
    for op in script_p:
        if op["op"] != "commit":
            apply_op(sess_fail, op)
    bad_note = sess_fail.working.part(pid).measures[1].notes[0]
    good_duration = bad_note.duration_ticks
    bad_note.duration_ticks = -5
    results_f = ParallelCommitter(sess_fail, max_workers=4, buffer_size=4) \
        .commit_all_pending()
    failed = [r for r in results_f if r.status == "failed"]
    check(len(failed) == 1 and failed[0].error.code == "E_DUR",
          f"单小节异常隔离 (failed={len(failed)}, code="
          f"{failed[0].error.code if failed else '-'})")
    sess_fail.set_note_duration(pid, 1, bad_note.note_id, good_duration)
    retry = ParallelCommitter(sess_fail, max_workers=2, buffer_size=2).commit(
        [(pid, 1)])
    check(retry[0].status == "committed", "失败小节修复后重试成功，其他小节未受污染")

    rep.section("7. 回放一致性与只读性")
    score_p, _ = load_project(sample)
    engine = PlaybackEngine(score_p)
    before = score_p.to_data()
    engine.play(from_tick=0)
    t0 = {e["event_id"]: e["abs_ms"] for e in engine.timeline}
    engine.pause()
    engine.play(from_tick=960)
    engine.advance(123.0)
    engine.pause()
    engine.play(from_tick=4800)
    t1 = {e["event_id"]: e["abs_ms"] for e in engine.timeline}
    check(t0 == t1, "切换起播点后事件绝对时间不漂移")
    check(score_p.to_data() == before, "回放不修改模型状态")
    rep.log(f"  回放时间轴事件数: {len(engine.timeline)}")

    rep.section("验收结论")
    rep.log(f"  失败项: {failures}")
    rep.log("  " + ("全部通过" if failures == 0 else "存在失败项，请检查上述 FAIL"))
    report_path = workdir / "acceptance_report.txt"
    rep.save(report_path)
    print(f"\n报告已写入 {report_path}")
    return 0 if failures == 0 else 1


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="ScoreForge 验收运行器")
    parser.add_argument("--workdir", default="artifacts/acceptance")
    args = parser.parse_args(argv)
    return run_acceptance(args.workdir)


if __name__ == "__main__":
    raise SystemExit(main())
