#!/usr/bin/env python3
"""Generate the deterministic 12-gateway sample used by the validation run."""

import hashlib
import json
import random
from pathlib import Path

SEED = 20261119
OUT = Path(__file__).resolve().parents[1] / "samples" / "gateways.json"


def main() -> None:
    rng = random.Random(SEED)
    gateways = []
    for index in range(1, 13):
        identifier = f"gw-{index:03d}"
        actual = "full_cone" if index % 2 else "port_restricted"
        declared = actual
        # One intentional lie proves mismatch annotation without rejection.
        if index == 3:
            declared = "port_restricted"
        suffix = hashlib.sha256(f"{SEED}:{identifier}".encode()).hexdigest()[:16]
        gateways.append(
            {
                "gateway_id": identifier,
                "device_identifier": f"camera-gateway-{suffix}",
                "private_address": f"10.30.{rng.randrange(1, 31)}.{index}:{9000 + index}",
                "nat_simulator": actual,
                "declared_nat_type": declared,
                "expected_mismatch": actual != declared,
            }
        )
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps({"seed": SEED, "gateways": gateways}, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
