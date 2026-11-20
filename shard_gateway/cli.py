"""Command-line interface for the local shard gateway."""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import sys
from pathlib import Path
from typing import Any

from .errors import GatewayError
from .recovery import recover
from .scheduler import ShardTask, schedule_shards
from .storage import FAULT_PHASES, UploadGateway
from .util import canonical_json_bytes, sha256_bytes
from .verifier import verify


def _emit(value: Any) -> None:
    sys.stdout.buffer.write(canonical_json_bytes(value))
    sys.stdout.buffer.flush()


def _fail(exc: GatewayError) -> int:
    sys.stderr.buffer.write(canonical_json_bytes({"error": exc.as_dict()}))
    sys.stderr.buffer.flush()
    return exc.exit_code


def _make_sample(path: Path, size: int, seed: str) -> dict[str, Any]:
    if size < 1:
        raise GatewayError("E_INVALID_ARGUMENT", "sample size must be positive")
    path.parent.mkdir(parents=True, exist_ok=True)
    digest = hashlib.sha256()
    with path.open("wb") as stream:
        remaining = size
        counter = 0
        while remaining:
            block = hashlib.sha256(f"{seed}:{counter}".encode("utf-8")).digest()
            counter += 1
            chunk = block[: min(remaining, len(block))]
            stream.write(chunk)
            digest.update(chunk)
            remaining -= len(chunk)
    return {"path": str(path), "size": size, "sha256": digest.hexdigest()}


def _read_sample(path: Path) -> bytes:
    try:
        return path.read_bytes()
    except OSError as exc:
        raise GatewayError("E_INVALID_ARGUMENT", f"cannot read sample: {exc}") from exc


def _upload(args: argparse.Namespace) -> dict[str, Any]:
    gateway = UploadGateway(args.root)
    data = _read_sample(Path(args.sample))
    total_shards = math.ceil(len(data) / args.shard_size)
    if total_shards < 1:
        raise GatewayError("E_INVALID_ARGUMENT", "empty objects are not supported")
    expected_digest = sha256_bytes(data)
    gateway.create_session(
        session_id=args.session_id,
        object_key=args.object_key,
        shard_size=args.shard_size,
        total_shards=total_shards,
        expected_size=len(data),
        expected_sha256=expected_digest,
        session_quota=args.session_quota,
    )
    current = gateway.get_session(args.session_id)
    if current["status"] == "committed":
        manifest = gateway.get_manifest_for_key(args.object_key)
        return {
            "session_id": args.session_id,
            "already_committed": True,
            "scheduler": None,
            "manifest": manifest,
        }
    tasks = [
        ShardTask(
            number=index // args.shard_size + 1,
            data=data[index : index + args.shard_size],
            expected_sha256=sha256_bytes(data[index : index + args.shard_size]),
            expected_length=len(data[index : index + args.shard_size]),
        )
        for index in range(0, len(data), args.shard_size)
    ]
    scheduler = schedule_shards(
        gateway,
        args.session_id,
        tasks,
        max_inflight=args.max_inflight,
        space_budget=args.space_budget,
        fault_phase=args.fault_phase,
    )
    manifest = gateway.commit_session(
        args.session_id, fault_phase=args.fault_phase
    )
    scheduler.pop("results", None)
    return {
        "session_id": args.session_id,
        "already_committed": False,
        "sample": {"size": len(data), "sha256": expected_digest},
        "scheduler": scheduler,
        "manifest": manifest,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m shard_gateway",
        description="Local deterministic sharded object upload gateway",
    )
    sub = parser.add_subparsers(dest="command", required=True)

    create = sub.add_parser("create", help="create or inspect an upload session")
    create.add_argument("--root", required=True)
    create.add_argument("--session-id", required=True)
    create.add_argument("--object-key", required=True)
    create.add_argument("--shard-size", type=int, required=True)
    create.add_argument("--total-shards", type=int, required=True)
    create.add_argument("--expected-size", type=int)
    create.add_argument("--expected-sha256")
    create.add_argument("--session-quota", type=int)

    put = sub.add_parser("put-shard", help="write one shard")
    put.add_argument("--root", required=True)
    put.add_argument("--session-id", required=True)
    put.add_argument("--shard-number", type=int, required=True)
    put.add_argument("--data-file", required=True)
    put.add_argument("--expected-sha256")
    put.add_argument("--fault-phase", choices=sorted(FAULT_PHASES))

    upload = sub.add_parser("upload", help="upload and atomically commit a sample")
    upload.add_argument("--root", required=True)
    upload.add_argument("--session-id", required=True)
    upload.add_argument("--object-key", required=True)
    upload.add_argument("--sample", required=True)
    upload.add_argument("--shard-size", type=int, required=True)
    upload.add_argument("--max-inflight", type=int, default=4)
    upload.add_argument("--space-budget", type=int, required=True)
    upload.add_argument("--session-quota", type=int)
    upload.add_argument("--fault-phase", choices=sorted(FAULT_PHASES))

    state = sub.add_parser("state", help="show session state")
    state.add_argument("--root", required=True)
    state.add_argument("--session-id", required=True)

    abort = sub.add_parser("abort", help="abort a session")
    abort.add_argument("--root", required=True)
    abort.add_argument("--session-id", required=True)

    rec = sub.add_parser("recover", help="scan and reconcile sessions after restart")
    rec.add_argument("--root", required=True)

    ver = sub.add_parser("verify", help="cross-check sessions and manifests")
    ver.add_argument("--root", required=True)

    man = sub.add_parser("manifest", help="show a published object manifest")
    man.add_argument("--root", required=True)
    man.add_argument("--object-key", required=True)

    sample = sub.add_parser("make-sample", help="generate deterministic sample bytes")
    sample.add_argument("--path", required=True)
    sample.add_argument("--size", type=int, required=True)
    sample.add_argument("--seed", default="shard-gateway-demo-v1")

    cleanup = sub.add_parser("cleanup", help="remove temporary/orphan shard files")
    cleanup.add_argument("--root", required=True)
    cleanup.add_argument("--session-id", required=True)
    cleanup.add_argument("--purge-committed", action="store_true")

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "make-sample":
            _emit(_make_sample(Path(args.path), args.size, args.seed))
            return 0
        if args.command == "create":
            gateway = UploadGateway(args.root)
            _emit(
                gateway.create_session(
                    session_id=args.session_id,
                    object_key=args.object_key,
                    shard_size=args.shard_size,
                    total_shards=args.total_shards,
                    expected_size=args.expected_size,
                    expected_sha256=args.expected_sha256,
                    session_quota=args.session_quota,
                )
            )
            return 0
        if args.command == "put-shard":
            gateway = UploadGateway(args.root)
            data = _read_sample(Path(args.data_file))
            _emit(
                gateway.write_shard(
                    session_id=args.session_id,
                    shard_number=args.shard_number,
                    data=data,
                    expected_sha256=args.expected_sha256,
                    fault_phase=args.fault_phase,
                )
            )
            return 0
        if args.command == "upload":
            _emit(_upload(args))
            return 0
        if args.command == "state":
            _emit(UploadGateway(args.root).get_session(args.session_id))
            return 0
        if args.command == "abort":
            _emit(UploadGateway(args.root).abort_session(args.session_id))
            return 0
        if args.command == "recover":
            report = recover(args.root)
            _emit(report)
            return 0 if report["ok"] else 2
        if args.command == "verify":
            report = verify(args.root)
            _emit(report)
            if report["error_codes"]:
                for code in report["error_codes"]:
                    sys.stderr.write(code + "\n")
                sys.stderr.flush()
                return 2
            return 0
        if args.command == "manifest":
            manifest = UploadGateway(args.root).get_manifest_for_key(args.object_key)
            if manifest is None:
                raise GatewayError(
                    "E_MANIFEST_INCOMPLETE", "no manifest exists for the object key"
                )
            _emit(manifest)
            return 0
        if args.command == "cleanup":
            gateway = UploadGateway(args.root)
            if args.purge_committed:
                result = gateway.purge_committed_session_files(args.session_id)
            else:
                result = gateway.cleanup_unreferenced(args.session_id)
            _emit({"session_id": args.session_id, **result})
            return 0
        parser.error("unknown command")
    except GatewayError as exc:
        return _fail(exc)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
