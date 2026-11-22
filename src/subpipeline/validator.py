"""Independent result verifier.

Reads only the persisted per-subtitle records and the project manifest, then
re-derives every check instead of trusting the main pipeline's in-memory
state. Returns the first violation with subtitle id, item and measured value.
"""

import argparse
import json
import os
import sys

from .checkpoint import RECORDS_DIR, _result_from_dict
from .errors import E_DECODE, E_TIMING, PipelineError
from .hashing import content_hash
from .loader import load_project, load_ground_truth
from .timing import readable_duration_s
from .version import ALGORITHM_VERSION


class Violation:
    def __init__(self, subtitle_id, item, measured, expected=None, code=E_TIMING):
        self.subtitle_id = subtitle_id
        self.item = item
        self.measured = measured
        self.expected = expected
        self.code = code

    def to_dict(self):
        return {
            "code": self.code,
            "subtitle_id": self.subtitle_id,
            "item": self.item,
            "measured": self.measured,
            "expected": self.expected,
        }


def load_persisted_records(run_dir):
    records_dir = os.path.join(run_dir, RECORDS_DIR)
    if not os.path.isdir(records_dir):
        raise PipelineError(E_DECODE, f"no records directory under {run_dir}")
    out = []
    for name in sorted(os.listdir(records_dir)):
        if not name.endswith(".json"):
            continue
        try:
            with open(os.path.join(records_dir, name), encoding="utf-8") as fh:
                out.append(json.load(fh))
        except (OSError, json.JSONDecodeError) as exc:
            raise PipelineError(E_DECODE, f"corrupt record {name}: {exc}")
    out.sort(key=lambda r: r["sequence"])
    return out


def verify_run(project_dir, run_dir, tolerance_s=0.02, truth=None):
    project, _manifest, _wave, _hash = load_project(project_dir)
    records = load_persisted_records(run_dir)
    if not records:
        raise PipelineError(E_DECODE, "no committed records to verify")

    # Version gate identical to the recovery rule.
    project_hash = content_hash(_manifest)
    params_hash = content_hash(project.params.to_dict())
    for rec in records:
        if rec.get("project_hash") != project_hash or \
                rec.get("params_hash") != params_hash or \
                rec.get("algorithm_version") != ALGORITHM_VERSION:
            return Violation(
                rec["result"]["subtitle_id"], "version_mismatch",
                {"project_hash": rec.get("project_hash"),
                 "params_hash": rec.get("params_hash"),
                 "algorithm_version": rec.get("algorithm_version")},
                {"project_hash": project_hash, "params_hash": params_hash,
                 "algorithm_version": ALGORITHM_VERSION},
                code=E_DECODE)

    results = [_result_from_dict(r["result"]) for r in records]
    sub_map = {s.subtitle_id: s for s in project.subtitles}
    if len(results) != len(project.subtitles):
        return Violation(
            None, "coverage",
            {"committed": len(results), "total": len(project.subtitles)})

    violation = _check_windows(results, project, sub_map)
    if violation:
        return violation
    violation = _check_read_speed(results, project, sub_map)
    if violation:
        return violation
    if truth is not None:
        violation = _check_truth(results, truth, project, tolerance_s)
        if violation:
            return violation
    return None


def _check_windows(results, project, sub_map):
    ordered = sorted(results, key=lambda r: r.start_sample)
    prev = None
    segment_map = project.segment_index()
    for result in ordered:
        if result.subtitle_id not in sub_map:
            return Violation(result.subtitle_id, "unknown_subtitle",
                             result.subtitle_id)
        if result.end_sample <= result.start_sample:
            return Violation(result.subtitle_id, "non_positive_window",
                             [result.start_sample, result.end_sample])
        if prev is not None and result.start_sample < prev.end_sample:
            return Violation(
                result.subtitle_id, "window_overlap",
                {"start": result.start_sample,
                 "previous_end": prev.end_sample})
        seg = segment_map[result.segment_ref]
        # Window may extend into gaps but may not cross into another segment.
        if result.end_sample <= seg.start_sample or result.start_sample >= seg.end_sample:
            return Violation(result.subtitle_id, "window_off_segment",
                             [result.start_sample, result.end_sample],
                             [seg.start_sample, seg.end_sample])
        prev = result
    return None


def _check_read_speed(results, project, sub_map):
    for result in results:
        sub = sub_map[result.subtitle_id]
        speed = sub.max_read_speed or project.params.default_read_speed
        required = readable_duration_s(
            result.screens, project.params, speed)
        actual = (result.end_sample - result.start_sample) / project.sample_rate
        # One sample of quantisation tolerance on each side.
        sample_tol = 1.0 / project.sample_rate
        if actual + sample_tol < required:
            return Violation(
                result.subtitle_id, "read_speed",
                {"actual_s": round(actual, 6), "required_s": round(required, 6)})
    return None


def _check_truth(results, truth, project, tolerance_s):
    truth_map = {e["subtitle_id"]: e for e in truth["entries"]}
    for result in results:
        entry = truth_map.get(result.subtitle_id)
        if entry is None:
            return Violation(result.subtitle_id, "truth_missing",
                             result.subtitle_id)
        if result.start_sample != entry["start_sample"] or \
                result.end_sample != entry["end_sample"]:
            return Violation(
                result.subtitle_id, "window",
                [result.start_sample, result.end_sample],
                [entry["start_sample"], entry["end_sample"]])
        flat_lines = result.all_lines
        expected_lines = [line for screen in entry["lines"] for line in screen]
        if list(flat_lines) != expected_lines:
            return Violation(result.subtitle_id, "wrapping",
                             list(flat_lines), expected_lines)
        line_counts = [s.line_count for s in result.screens]
        if line_counts != entry["line_counts"]:
            return Violation(result.subtitle_id, "line_counts",
                             line_counts, entry["line_counts"])
        char_counts = [[len(line) for line in s.lines] for s in result.screens]
        if char_counts != entry["line_char_counts"]:
            return Violation(result.subtitle_id, "line_char_counts",
                             char_counts, entry["line_char_counts"])
        if abs(result.actual_duration_s - entry["actual_duration_s"]) > tolerance_s:
            return Violation(
                result.subtitle_id, "duration",
                result.actual_duration_s, entry["actual_duration_s"])
        # Cross-check trade-off records against the persisted main-chain copy.
        if [t.to_dict() for t in result.tradeoffs] != entry["tradeoffs"]:
            return Violation(result.subtitle_id, "tradeoff_records",
                             [t.to_dict() for t in result.tradeoffs],
                             entry["tradeoffs"])
    total = (max(r.end_sample for r in results)
             - min(r.start_sample for r in results)) / project.sample_rate
    if abs(total - truth["total_occupied_s"]) > tolerance_s:
        return Violation(None, "total_occupied_s", round(total, 6),
                         truth["total_occupied_s"])
    return None


def main(argv=None):
    parser = argparse.ArgumentParser(description="Verify persisted subtitle run")
    parser.add_argument("project_dir")
    parser.add_argument("run_dir")
    parser.add_argument("--tolerance", type=float, default=0.02)
    parser.add_argument("--truth", default=None)
    args = parser.parse_args(argv)
    truth = args.truth and json.load(open(args.truth, encoding="utf-8")) or \
        load_ground_truth(args.project_dir)
    try:
        violation = verify_run(args.project_dir, args.run_dir,
                               args.tolerance, truth)
    except PipelineError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2))
        return 2
    if violation is None:
        print(json.dumps({"status": "ok"}, ensure_ascii=False))
        return 0
    print(json.dumps(violation.to_dict(), ensure_ascii=False, indent=2))
    return 1


if __name__ == "__main__":
    sys.exit(main())
