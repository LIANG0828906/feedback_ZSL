#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

import grpc

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from inference_router.pb import inference_pb2, inference_pb2_grpc  # noqa: E402


def set_delay(target: str, delay_ms: int) -> dict:
    with grpc.insecure_channel(target) as channel:
        reply = inference_pb2_grpc.WorkerInferenceStub(channel).SetFault(
            inference_pb2.FaultRequest(delay_ms=delay_ms),
            timeout=2.0,
        )
        return {"ok": bool(reply.ok), "delay_ms": int(reply.delay_ms), "target": target}


def main() -> int:
    parser = argparse.ArgumentParser(description="Inject replica processing delay")
    parser.add_argument("--target", required=True)
    parser.add_argument("--delay-ms", type=int, required=True)
    args = parser.parse_args()
    print(json.dumps(set_delay(args.target, args.delay_ms), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
