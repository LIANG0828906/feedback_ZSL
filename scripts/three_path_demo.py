"""Demonstrate serial/parallel/checkpoint-resume produce identical results."""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))

from subpipeline.loader import load_project
from subpipeline.pipeline import Pipeline


def main(project_dir, runs_root):
    project, manifest, wave, _h = load_project(project_dir)
    serial = Pipeline(project, manifest, wave, runs_root=runs_root, max_workers=1)
    s_results, _ = serial.run(parallel=False)

    parallel = Pipeline(project, manifest, wave, runs_root=runs_root,
                        max_workers=4, max_buffers=4)
    p_results, _ = parallel.run(parallel=True)

    resumed = Pipeline(project, manifest, wave, runs_root=runs_root,
                       max_workers=3, run_dir=parallel.run_dir)
    r_results, _ = resumed.run(parallel=True)

    payloads = [[r.to_dict() for r in rows]
                for rows in (s_results, p_results, r_results)]
    assert payloads[0] == payloads[1] == payloads[2], "three paths diverged"
    print(f"OK: serial / parallel / resume identical for "
          f"{len(payloads[0])} committed subtitles")
    print(f"run dir: {parallel.run_dir}")


if __name__ == "__main__":
    project = sys.argv[1] if len(sys.argv) > 1 else "samples/proj_main_20261123"
    runs = sys.argv[2] if len(sys.argv) > 2 else "runs_demo"
    main(project, runs)
