#!/usr/bin/env python3
import hashlib
import json
import pathlib
import sys


def sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> int:
    if len(sys.argv) != 3:
        print("usage: verify_baselines.py MANIFEST RUNS_DIR", file=sys.stderr)
        return 2
    manifest_path = pathlib.Path(sys.argv[1])
    runs_dir = pathlib.Path(sys.argv[2])
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    failures = []
    for sample in manifest["samples"]:
        name = pathlib.Path(sample["file"]).stem
        coverage_name = (
            "coverage_nonzero.pgm"
            if sample["fillRule"] == "nonzero"
            else "coverage_evenodd.pgm"
        )
        actual = sha256(runs_dir / name / "a" / coverage_name)
        if actual != sample["baselineCoverageHash"]:
            failures.append(
                f"{name}: {actual} != {sample['baselineCoverageHash']}"
            )
        if sample["coverageNonzeroHash"] != sha256(
            runs_dir / name / "a" / "coverage_nonzero.pgm"
        ):
            failures.append(f"{name}: nonzero coverage hash mismatch")
        if sample["coverageEvenOddHash"] != sha256(
            runs_dir / name / "a" / "coverage_evenodd.pgm"
        ):
            failures.append(f"{name}: even-odd coverage hash mismatch")
        result_path = runs_dir / name / "a" / "result.json"
        result = json.loads(result_path.read_text(encoding="utf-8"))
        analysis = result["budgetAnalysis"]
        expected_order = [
            "intersection_classify",
            "sample_distance",
            "vertex_merge",
        ]
        if analysis["tighteningOrder"] != expected_order:
            failures.append(f"{name}: tightening order changed")
        if len(analysis["impacts"]) != 3:
            failures.append(f"{name}: missing tolerance impact records")
        if "toleranceComparison" not in analysis:
            failures.append(f"{name}: missing tolerance comparison")
        metrics = result["metrics"]
        for key in (
            "ruleSensitivePixels",
            "uncertainPixels",
            "degraded",
            "committable",
        ):
            if key not in metrics:
                failures.append(f"{name}: missing metric {key}")
        if len(result["contentHash"]) != 64 or len(result["traceHash"]) != 64:
            failures.append(f"{name}: invalid content or trace hash")
        pixel_trace = runs_dir / name / "a" / "pixel_trace.jsonl"
        line_count = sum(1 for _ in pixel_trace.open("rb"))
        if line_count != 64 * 64:
            failures.append(f"{name}: pixel trace does not cover every pixel")
    if failures:
        for failure in failures:
            print(failure, file=sys.stderr)
        return 1
    print(f"baseline ok: {len(manifest['samples'])} samples")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
