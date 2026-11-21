"""Command line entry points."""
from __future__ import annotations

import argparse
import json
import sys

from .config import load_config
from .engine import run_bake
from .errors import BakeError
from .samplegen import generate_baseline, generate_samples
from .spectrum import load_materials


def _cmd_generate_samples(args):
    paths = generate_samples(args.out)
    print(f"samples written: {paths['materials']} / {paths['config']}")


def _cmd_baseline(args):
    baseline = generate_baseline(args.samples)
    print(f"baseline chain tip: {baseline['chain_tip']}")


def _cmd_bake(args):
    config = load_config(args.config)
    materials = load_materials(args.materials, config.grid)
    manifest = run_bake(
        config, materials, args.batch_id, simulate_kill_after=args.simulate_kill_after
    )
    print(json.dumps({"batch_id": manifest["batch_id"], "chain_tip": manifest["hash_chain"][-1]}))


def _cmd_gui(args):
    from .gui import create_app

    app, window = create_app(args.manifest, args.trace)
    window.show()
    sys.exit(app.exec())


def _cmd_acceptance(args):
    from .acceptance import run_acceptance

    results = run_acceptance(args.workdir)
    print(json.dumps(results, indent=1, sort_keys=True))
    if not all(r["ok"] for r in results.values()):
        sys.exit(1)


def main(argv=None):
    parser = argparse.ArgumentParser(prog="spectral_bake")
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("generate-samples")
    p.add_argument("--out", required=True)
    p.set_defaults(func=_cmd_generate_samples)

    p = sub.add_parser("baseline")
    p.add_argument("--samples", required=True)
    p.set_defaults(func=_cmd_baseline)

    p = sub.add_parser("bake")
    p.add_argument("--config", required=True)
    p.add_argument("--materials", required=True)
    p.add_argument("--batch-id", required=True)
    p.add_argument("--simulate-kill-after", type=int, default=None)
    p.set_defaults(func=_cmd_bake)

    p = sub.add_parser("gui")
    p.add_argument("--manifest", required=True)
    p.add_argument("--trace", default=None)
    p.set_defaults(func=_cmd_gui)

    p = sub.add_parser("acceptance")
    p.add_argument("--workdir", required=True)
    p.set_defaults(func=_cmd_acceptance)

    args = parser.parse_args(argv)
    try:
        args.func(args)
        return 0
    except BakeError as exc:
        print(str(exc), file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
