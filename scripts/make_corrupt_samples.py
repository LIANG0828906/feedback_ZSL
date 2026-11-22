#!/usr/bin/env python3
"""生成故意损坏的样本副本用于异常码验证(只改数据副本, 不改源码)。"""

import argparse
import json
import os
import shutil
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))


def _copy(src: str, dst: str) -> str:
    if os.path.isdir(dst):
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    return dst


def _edit_manifest(proj_dir: str, fn) -> None:
    path = os.path.join(proj_dir, "manifest.json")
    with open(path, "r", encoding="utf-8") as f:
        doc = json.load(f)
    fn(doc)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(doc, f, ensure_ascii=False, indent=1)


def make_all(src: str, out: str) -> dict:
    os.makedirs(out, exist_ok=True)
    made = {}

    def variant(name: str, fn) -> None:
        dst = _copy(src, os.path.join(out, name))
        _edit_manifest(dst, fn)
        made[name] = dst

    variant("missing_field", lambda d: d.pop("frame_len"))
    variant("bad_segment_ref", lambda d: d["subtitles"][0].update(segment=999))
    variant("window_out_of_segment", lambda d: d["subtitles"][0].update(
        window=[d["segments"][d["subtitles"][0]["segment"]]["start"],
                d["segments"][d["subtitles"][0]["segment"]]["end"] + 10**6]))
    variant("bad_speed", lambda d: d["assist"].update(speed=-1.5))
    variant("nan_speed", lambda d: d["assist"].update(speed=float("nan")))
    variant("zero_weights", lambda d: d["assist"]["char_weights"].__setitem__(
        0, [0] * len(d["assist"]["char_weights"][0])))
    variant("bad_hop", lambda d: d.update(hop=d["frame_len"] * 2))
    variant("bad_total_samples", lambda d: d.update(total_samples=d["total_samples"] + 1))
    return made


def main() -> None:
    ap = argparse.ArgumentParser(description="生成损坏样本副本")
    ap.add_argument("--src", default="data/samples/proj_000")
    ap.add_argument("--out", default="data/corrupt")
    args = ap.parse_args()
    made = make_all(args.src, args.out)
    for name, path in made.items():
        print(f"corrupt[{name}]: {path}")


if __name__ == "__main__":
    main()
