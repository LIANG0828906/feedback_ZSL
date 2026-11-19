"""Download duckdb / pyarrow wheels for offline Windows installation.

Run on a network-connected machine (any OS) targeting the Windows Python 3.11
tags, then copy the ``wheels/`` directory to the air-gapped Windows host.
"""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="wheels")
    parser.add_argument(
        "--python-tag", default="cp311", help="wheel Python tag, e.g. cp311"
    )
    parser.add_argument(
        "--platform",
        default="win_amd64",
        help="wheel platform tag, e.g. win_amd64 or manylinux2014_x86_64",
    )
    args = parser.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    cmd = [
        sys.executable,
        "-m",
        "pip",
        "download",
        "-r",
        "requirements.txt",
        "-d",
        str(out),
        "--only-binary=:all:",
        "--python-version",
        args.python_tag.replace("cp", ""),
        "--platform",
        args.platform,
        "--implementation",
        "cp",
        "--abi",
        args.python_tag,
    ]
    return subprocess.call(cmd)


if __name__ == "__main__":
    raise SystemExit(main())
