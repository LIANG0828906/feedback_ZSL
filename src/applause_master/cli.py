"""Command line entry points for synthesis and master rendering."""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys

from .errors import BusinessError
from .pipeline import run_pipeline
from .project import load_project
from .synth import generate_project_audio


def build_demo_project(work_dir, seed=20261118, duration_seconds=20.0):
    """Create a self-contained six-microphone demo project on disk."""
    os.makedirs(work_dir, exist_ok=True)
    mic_dir = os.path.join(work_dir, "mics")
    os.makedirs(mic_dir, exist_ok=True)
    positions = [
        (-9.0, 6.0, 1.6),
        (-4.5, 3.5, 1.7),
        (0.0, 2.5, 2.0),
        (4.5, 3.5, 1.7),
        (9.0, 6.0, 1.6),
        (0.0, 10.0, 3.0),
    ]
    microphones = []
    for index, position in enumerate(positions):
        name = f"mic_{index + 1}"
        microphones.append({
            "name": name,
            "file": f"mics/{name}.wav",
            "channels": 1,
            "position": list(position),
        })
    project = {
        "seed": seed,
        "sample_rate": 48000,
        "duration_seconds": duration_seconds,
        "microphones": microphones,
        "detection_threshold": 0.012,
        "freq_bands": [
            {"name": "low", "low_hz": 20.0, "high_hz": 250.0},
            {"name": "mid", "low_hz": 250.0, "high_hz": 2000.0},
            {"name": "high", "low_hz": 2000.0, "high_hz": 8000.0},
            {"name": "air", "low_hz": 8000.0, "high_hz": 20000.0},
        ],
        "target_loudness_lufs": -23.0,
        "true_peak_limit_dbtp": -1.0,
        "energy_gate": 0.05,
        "noise_gain_db": -6.0,
        "block_seconds": 0.5,
    }
    project_path = os.path.join(work_dir, "project.json")
    with open(project_path, "w", encoding="utf-8") as handle:
        json.dump(project, handle, indent=2, sort_keys=False)
    return project_path


def cmd_generate(args):
    project = load_project(args.project)
    truth = generate_project_audio(project)
    truth_path = os.path.join(os.path.dirname(project["project_path"]), "ground_truth.json")
    with open(truth_path, "w", encoding="utf-8") as handle:
        json.dump(truth, handle, indent=2, sort_keys=False)
    print(json.dumps({"status": "generated", "microphones": len(project["microphones"]),
                      "ground_truth": os.path.basename(truth_path),
                      "event_count": truth["event_count"]}, indent=2))
    return 0


def cmd_render(args):
    interrupt_flag = {"fired": False}

    def _handler(signum, frame):
        interrupt_flag["fired"] = True

    previous = signal.signal(signal.SIGINT, _handler)
    try:
        project = load_project(args.project)
        result = run_pipeline(
            project, args.output,
            interrupted=(lambda: interrupt_flag["fired"]) if not args.no_interrupt else None,
        )
    except BusinessError as exc:
        payload = {"status": "rejected", **exc.to_dict()}
        print(json.dumps(payload, indent=2, ensure_ascii=False), file=sys.stderr)
        return 2
    finally:
        signal.signal(signal.SIGINT, previous)
    print(json.dumps(result, indent=2, ensure_ascii=False))
    return int(result.get("exit_code", 1))


def cmd_demo(args):
    project_path = build_demo_project(args.workdir, seed=args.seed,
                                      duration_seconds=args.duration)
    # The freshly scaffolded project references WAVs that do not exist yet;
    # synthesize them first, then run the same validated entry path as render.
    project = _load_unchecked_demo(project_path)
    truth = generate_project_audio(project)
    project = load_project(project_path)
    with open(os.path.join(args.workdir, "ground_truth.json"), "w", encoding="utf-8") as handle:
        json.dump(truth, handle, indent=2, sort_keys=False)
    result = run_pipeline(project, os.path.join(args.workdir, "out"))
    print(json.dumps({"project": project_path, **result}, indent=2, ensure_ascii=False))
    return int(result.get("exit_code", 1))


def _load_unchecked_demo(project_path):
    with open(project_path, "r", encoding="utf-8") as handle:
        data = json.load(handle)
    base = os.path.dirname(os.path.abspath(project_path))
    for mic in data["microphones"]:
        if not os.path.isabs(mic["file"]):
            mic["file"] = os.path.join(base, mic["file"])
    data["project_path"] = project_path
    return data


def build_parser():
    parser = argparse.ArgumentParser(description="Deterministic applause master pipeline")
    sub = parser.add_subparsers(dest="command", required=True)

    gen = sub.add_parser("generate", help="synthesize microphone WAVs from a project JSON")
    gen.add_argument("project")
    gen.set_defaults(func=cmd_generate)

    render = sub.add_parser("render", help="detect events, render and check the master")
    render.add_argument("project")
    render.add_argument("--output", required=True)
    render.add_argument("--no-interrupt", action="store_true")
    render.set_defaults(func=cmd_render)

    demo = sub.add_parser("demo", help="scaffold, synthesize and render a full demo")
    demo.add_argument("workdir")
    demo.add_argument("--seed", type=int, default=20261118)
    demo.add_argument("--duration", type=float, default=20.0)
    demo.set_defaults(func=cmd_demo)
    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
