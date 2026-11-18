from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from rec_gateway.scenario import ScenarioOptions, run_scenario


def main() -> int:
    parser = argparse.ArgumentParser(description="Run a deterministic gateway scenario")
    parser.add_argument("--output", default=str(ROOT / "reports" / "scenario.json"))
    parser.add_argument("--tenant", default="tenant-d")
    parser.add_argument(
        "--protocol-version",
        choices=("v3", "v4", "mixed"),
        default="mixed",
    )
    parser.add_argument("--count", type=int, default=100)
    parser.add_argument("--rate", type=float, default=20.0)
    parser.add_argument("--delay-rate", type=float, default=0.0)
    parser.add_argument("--delay-ms", type=float, default=0.0)
    parser.add_argument("--misalign-rate", type=float, default=0.0)
    parser.add_argument("--canary-weight", type=float, default=1.0)
    parser.add_argument("--seed", type=int, default=294)
    parser.add_argument("--samples", default=str(ROOT / "generated" / "samples" / "requests.jsonl"))
    parser.add_argument("--models-dir", default=str(ROOT / "generated" / "models"))
    parser.add_argument("--base-url", default=None)
    parser.add_argument("--inject-load-failure", action="store_true")
    parser.add_argument("--timeout-seconds", type=float, default=5.0)
    args = parser.parse_args()

    for name in ("delay_rate", "misalign_rate", "canary_weight"):
        value = getattr(args, name)
        if value < 0.0 or value > 1.0:
            parser.error(f"--{name.replace('_', '-')} must be within [0, 1]")
    if args.count < 1:
        parser.error("--count must be positive")
    if args.rate < 0:
        parser.error("--rate cannot be negative")

    options = ScenarioOptions(
        output=Path(args.output).resolve(),
        tenant_id=args.tenant,
        protocol_version=args.protocol_version,
        count=args.count,
        rate=args.rate,
        delay_rate=args.delay_rate,
        delay_ms=args.delay_ms,
        misalign_rate=args.misalign_rate,
        canary_weight=args.canary_weight,
        seed=args.seed,
        samples_path=Path(args.samples).resolve(),
        models_dir=Path(args.models_dir).resolve(),
        base_url=args.base_url,
        inject_load_failure=args.inject_load_failure,
        timeout_seconds=args.timeout_seconds,
    )
    report = run_scenario(options)
    print(json.dumps(report, ensure_ascii=False, sort_keys=True, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
