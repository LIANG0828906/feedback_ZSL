from __future__ import annotations

import argparse
import json
import sys
import urllib.request
from pathlib import Path

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from train.common import load_config


def main() -> int:
    parser = argparse.ArgumentParser(description="Fetch coordinator JSON status")
    parser.add_argument("--config", default="config.json")
    parser.add_argument("--metrics", action="store_true")
    args = parser.parse_args()
    config = load_config(args.config)
    coordinator = config["coordinator"]
    path = "/metrics" if args.metrics else "/status"
    url = f"http://{coordinator['host']}:{coordinator['status_port']}{path}"
    with urllib.request.urlopen(url, timeout=5.0) as response:
        body = response.read().decode("utf-8")
    if args.metrics:
        print(body)
    else:
        print(json.dumps(json.loads(body), indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
