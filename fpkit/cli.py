"""Command line entry: generate / build / query / experiment."""

import argparse
import json
import sys

from .config import load_config
from .errors import FpError


def main(argv=None):
    parser = argparse.ArgumentParser(prog="fpkit")
    sub = parser.add_subparsers(dest="cmd", required=True)
    p_gen = sub.add_parser("generate")
    p_gen.add_argument("--workdir", required=True)
    p_gen.add_argument("--seed", type=int, default=20261122)
    p_build = sub.add_parser("build")
    p_build.add_argument("--workdir", required=True)
    p_build.add_argument("--config")
    p_build.add_argument("--batch-id", default="batch-1")
    p_build.add_argument("--batch-seq", type=int, default=1)
    p_build.add_argument("--trace-name", default="trace_build.jsonl")
    p_build.add_argument("--kill-after", type=int)
    p_q = sub.add_parser("query")
    p_q.add_argument("--workdir", required=True)
    p_q.add_argument("--config")
    p_exp = sub.add_parser("experiment")
    p_exp.add_argument("--root", required=True)
    p_exp.add_argument("--seed", type=int, default=20261122)
    args = parser.parse_args(argv)

    from . import experiment as exp
    try:
        if args.cmd == "generate":
            exp.generate(args.workdir, args.seed)
        elif args.cmd == "build":
            cfg = load_config(args.config) if args.config else load_config(
                f"{args.workdir}/config.json")
            summary = exp.build(args.workdir, cfg, args.batch_id,
                                args.batch_seq, args.trace_name,
                                args.kill_after)
            print(json.dumps(summary, sort_keys=True))
        elif args.cmd == "query":
            cfg = load_config(args.config) if args.config else load_config(
                f"{args.workdir}/config.json")
            exp.query(args.workdir, cfg)
        elif args.cmd == "experiment":
            summary = exp.run_full_experiment(args.root, args.seed)
            for c in summary["checks"]:
                mark = "PASS" if c["ok"] else "FAIL"
                detail = f"  ({c['detail']})" if c["detail"] else ""
                print(f"[{mark}] {c['name']}{detail}")
            print(f"ALL_OK={summary['all_ok']}")
            return 0 if summary["all_ok"] else 1
    except FpError as exc:
        print(json.dumps(exc.to_dict(), sort_keys=True), file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
