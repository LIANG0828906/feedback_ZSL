"""强制终止（SIGKILL）后续算的可复现验证脚本。

流程：
1. 用样本输入启动一个只推进到中途就自我 SIGKILL 的子进程；
2. 检查磁盘上保留的是最后一个完整提交步的检查点；
3. 从该检查点续算到结束；
4. 与一次完整离线重演对比逐步哈希与累计配额，必须完全一致。
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import tempfile

from replay_service.checkpoint import CheckpointStore
from replay_service.engine import QuotaReplayEngine
from replay_service.parser import parse_input_file
from replay_service.runner import replay_exercise


KILL_MARKER_STEP = 5


def _worker(input_path: str, cp_dir: str):
    config, operations = parse_input_file(input_path)
    store = CheckpointStore(cp_dir)
    engine = QuotaReplayEngine(config, operations)
    while engine.step_index < config.total_steps:
        engine.step()
        store.write(config, engine.snapshot())
        if engine.step_index == KILL_MARKER_STEP:
            os.kill(os.getpid(), signal.SIGKILL)


def main() -> int:
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    sample_input = os.path.join(
        root, "samples", "run1", "large_degrade", "input.json"
    )
    if not os.path.exists(sample_input):
        print("请先运行: python -m replay_service.cli generate --out-dir samples/run1")
        return 1

    work = tempfile.mkdtemp(prefix="kill-resume-")
    cp_dir = os.path.join(work, "cp")

    proc = subprocess.run(
        [
            sys.executable,
            "-c",
            (
                "import sys; sys.path.insert(0, %r); "
                "from scripts.verify_kill_resume import _worker; "
                "_worker(sys.argv[1], sys.argv[2])"
            )
            % root,
            sample_input,
            cp_dir,
        ],
        capture_output=True,
        text=True,
    )
    assert proc.returncode < 0, "子进程应被信号杀死"

    config, operations = parse_input_file(sample_input)
    store = CheckpointStore(cp_dir)
    killed_payload = store.load_raw()
    assert killed_payload["step_index"] == KILL_MARKER_STEP, (
        f"检查点停在 {killed_payload['step_index']}，期望 {KILL_MARKER_STEP}"
    )

    resumed = replay_exercise(
        config,
        operations,
        checkpoint_dir=cp_dir,
        trajectory_path=os.path.join(work, "resumed.jsonl"),
        resume=True,
    )
    full = replay_exercise(
        config,
        operations,
        checkpoint_dir=os.path.join(work, "cp_full"),
        trajectory_path=os.path.join(work, "full.jsonl"),
        resume=False,
    )

    resumed_hashes = [r.step_hash for r in resumed.results]
    full_hashes = [r.step_hash for r in full.results]
    assert resumed_hashes == full_hashes, "续算后哈希链与完整重演不一致"
    assert resumed.results[-1].cumulative_used == full.results[-1].cumulative_used
    assert (
        resumed.results[-1].cumulative_penalty
        == full.results[-1].cumulative_penalty
    )

    print(
        json.dumps(
            {
                "killed_at_step": KILL_MARKER_STEP,
                "total_steps": config.total_steps,
                "resumed_steps": len(resumed.results),
                "hash_chain_match": True,
                "final_cumulative_used": resumed.results[-1].cumulative_used,
                "last_hash": resumed.results[-1].step_hash,
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
