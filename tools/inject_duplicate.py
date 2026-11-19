from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from proto import training_pb2 as pb
from train.client import coordinator_stub
from train.common import load_config
from train.db import connect, database_path


def inject_duplicate(
    config_path: str, worker_id: str | None = None, step: int | None = None
) -> dict:
    config = load_config(config_path)
    connection = connect(database_path(config))
    try:
        row = None
        if step is not None:
            row = connection.execute(
                """
                SELECT * FROM submissions
                WHERE status = 'applied' AND step = ?
                  AND (? IS NULL OR worker_id = ?)
                ORDER BY worker_id LIMIT 1
                """,
                (int(step), worker_id, worker_id),
            ).fetchone()
        if row is None:
            row = connection.execute(
                """
                SELECT * FROM submissions
                WHERE status = 'applied' AND (? IS NULL OR worker_id = ?)
                ORDER BY step DESC, worker_id LIMIT 1
                """,
                (worker_id, worker_id),
            ).fetchone()
        if row is None:
            raise RuntimeError("no applied submission is available for duplicate injection")
        submission = dict(row)
    finally:
        connection.close()
    channel, stub = coordinator_stub(config)
    try:
        reply = stub.SubmitGradient(
            pb.GradientSubmission(
                worker_id=submission["worker_id"],
                step=int(submission["step"]),
                base_version=int(submission["base_version"]),
                sample_count=int(submission["sample_count"]),
                gradient=list(
                    __import__("numpy").frombuffer(
                        submission["gradient"], dtype="float64"
                    ).tolist()
                ),
                local_loss=float(submission["local_loss"]),
                client_started_ms=0.0,
            ),
            timeout=10.0,
        )
    finally:
        channel.close()
    return {
        "applied_version": int(reply.applied_version),
        "code": reply.code,
        "discarded": bool(reply.discarded),
        "message": reply.message,
        "ok": bool(reply.ok),
        "status": reply.status,
        "step": int(submission["step"]),
        "worker_id": submission["worker_id"],
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Inject a duplicate applied gradient")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--worker-id", default=None)
    parser.add_argument("--step", type=int, default=None)
    args = parser.parse_args()
    result = inject_duplicate(args.config, args.worker_id, args.step)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0 if result["code"] == "DUPLICATE_GRADIENT" else 1


if __name__ == "__main__":
    raise SystemExit(main())
