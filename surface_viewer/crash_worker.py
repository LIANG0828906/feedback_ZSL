"""Child process used to prove forced-termination recovery.

The process is intentionally terminated with ``os._exit`` after a chosen
number of artifact renames and before the manifest rename.  On Windows this is
the same hard termination semantics as ``TerminateProcess`` (and can also be
triggered externally with ``taskkill /PID <pid> /F``).
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

from .model import load_assembly
from .runtime import SurfaceRuntime
from .samplegen import operation_changes


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--assembly", required=True)
    parser.add_argument("--store", required=True)
    parser.add_argument("--operations", required=True)
    parser.add_argument("--operation-index", type=int, default=0)
    parser.add_argument("--crash-after-artifacts", type=int, default=1)
    args = parser.parse_args()

    assembly = load_assembly(args.assembly)
    runtime = SurfaceRuntime.bootstrap(
        assembly,
        args.store,
        verify_full=False,
    )
    operations = json.loads(
        Path(args.operations).read_text(encoding="utf-8")
    )["operations"]
    operation = operations[args.operation_index]
    patch_id, changes = operation_changes(runtime.engine.assembly, operation)
    runtime.engine.update(patch_id, changes, verify_full=False)
    runtime.store.commit(
        runtime.engine.assembly,
        runtime.engine.graph,
        runtime.engine.cache,
        expected_previous_commit_seq=runtime.commit_seq,
        crash_after_artifact_writes=args.crash_after_artifacts,
    )
    os._exit(24)


if __name__ == "__main__":
    raise SystemExit(main())
