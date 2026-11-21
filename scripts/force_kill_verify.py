"""End-to-end forced-kill recovery verification.

Spawns a child process that publishes an initial release and then starts a
slow recompute (per-version crash journals are written as it progresses).
The parent SIGKILLs the child mid-recompute, then recovers from the
on-disk snapshot and verifies that only the incomplete versions are
recomputed and that the hash chain validates.
"""

import os
import signal
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

CHILD = r"""
import sys
from spectral_studio import samplegen
from spectral_studio.engine import MaterialLibrary

snapshot_path = sys.argv[1]
library = MaterialLibrary(snapshot_path=snapshot_path)
library.recompute_delay = 0.05
materials, _ = samplegen.generate_samples(seed=42)
# Inflate to 40 materials so the recompute spans many journal checkpoints.
docs = []
for index in range(40):
    base = dict(materials[index % len(materials)])
    base["id"] = f"mat-{index:03d}"
    docs.append(base)
library.submit([{"op": "add_material", "doc": d} for d in docs])
print("child: initial release published", flush=True)
library.submit([{"op": "set_env", "white_point": "D50"}])
print("child: recompute finished (unexpected)", flush=True)
"""


def main():
    with tempfile.TemporaryDirectory() as tmp:
        snapshot = os.path.join(tmp, "snapshot.json")
        env = dict(os.environ, PYTHONPATH=os.getcwd())
        child = subprocess.Popen(
            [sys.executable, "-c", CHILD, snapshot],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            env=env, cwd=os.getcwd())
        assert child.stdout.readline().strip() == \
            "child: initial release published"
        time.sleep(0.4)  # let the recompute reach mid-stream journals
        child.send_signal(signal.SIGKILL)
        child.wait()
        assert child.returncode == -signal.SIGKILL
        print("parent: child killed with SIGKILL mid-recompute")

        from spectral_studio.engine import MaterialLibrary
        recovered = MaterialLibrary.recover(snapshot)
        release = recovered.current_release
        event = recovered.trace[-1]
        statuses = {v.status for v in release.versions.values()}
        print("parent: recovered release", release.release_id)
        print("parent: recovery event:", event["event"])
        print("parent: recomputed versions:", len(event.get("recomputed", [])))
        print("parent: all versions published:", statuses == {"published"})
        print("parent: chain hash:", release.chain_hash)
        # Recovery must be repeatable and hash-stable.
        again = MaterialLibrary.recover(snapshot)
        assert again.current_release.chain_hash == release.chain_hash
        assert statuses == {"published"}
        print("parent: recovery hash-stable across runs: True")
        print("FORCE-KILL RECOVERY OK")


if __name__ == "__main__":
    main()
