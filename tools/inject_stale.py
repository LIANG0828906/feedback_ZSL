from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from proto import training_pb2 as pb
from train.client import coordinator_stub
from train.common import load_config, load_manifest_for_config, parameter_names


def inject_stale(
    config_path: str,
    worker_id: str,
    count: int = 1,
    step: int | None = None,
) -> list[dict]:
    config = load_config(config_path)
    manifest = load_manifest_for_config(config)
    names = parameter_names(manifest)
    configured_steps = int(manifest["hyperparameters"]["steps"])
    max_stale = int(config["runtime"]["max_stale_steps"])
    channel, stub = coordinator_stub(config)
    results = []
    try:
        for _ in range(count):
            runtime = stub.GetRuntime(pb.RuntimeRequest(timeout_ms=1000), timeout=8.0)
            current = int(runtime.version)
            target_step = step or min(configured_steps, current + max_stale + 2)
            base_version = max(0, current - max_stale - 2)
            reply = stub.SubmitGradient(
                pb.GradientSubmission(
                    worker_id=worker_id,
                    step=target_step,
                    base_version=base_version,
                    sample_count=64,
                    gradient=[0.0] * len(names),
                    local_loss=0.0,
                    client_started_ms=time.time() * 1000.0,
                ),
                timeout=10.0,
            )
            results.append(
                {
                    "base_version": base_version,
                    "code": reply.code,
                    "current_version": current,
                    "message": reply.message,
                    "ok": bool(reply.ok),
                    "step": target_step,
                    "worker_id": worker_id,
                }
            )
            time.sleep(0.03)
    finally:
        channel.close()
    return results


def main() -> int:
    parser = argparse.ArgumentParser(description="Inject stale gradient submissions")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--worker-id", default="worker-1")
    parser.add_argument("--count", type=int, default=4)
    parser.add_argument("--step", type=int, default=None)
    args = parser.parse_args()
    results = inject_stale(args.config, args.worker_id, args.count, args.step)
    print(json.dumps(results, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
