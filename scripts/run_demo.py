from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from mediaforge.injection import InjectionConfig
from mediaforge.jsonutil import sha256_bytes
from mediaforge.package import build_package, write_package
from mediaforge.pipeline import load_sample, truth_path
from mediaforge.segmented import pack_segmented
from mediaforge.validator import validate_artifact
from mediaforge.errors import WorkbenchError


def main() -> int:
    output = ROOT / "artifacts" / "demo"
    checkpoints = ROOT / "artifacts" / "demo-checkpoints"
    shutil.rmtree(output, ignore_errors=True)
    shutil.rmtree(checkpoints, ignore_errors=True)
    output.mkdir(parents=True, exist_ok=True)
    sample = ROOT / "samples" / "g01_neon_city"
    report: dict[str, object] = {"sample": str(sample), "exports": [], "paths": {}}
    model = load_sample(sample, "v3").model
    for target in ("v1", "v2", "v3"):
        direct = build_package(model, target)
        first = output / f"g01_v3_to_{target}_direct_a.mfz"
        second = output / f"g01_v3_to_{target}_direct_b.mfz"
        write_package(direct, first)
        write_package(direct, second)
        validation = validate_artifact(first, truth_path(sample))
        report["exports"].append(
            {
                "target": target,
                "sha256": direct.artifact_sha256,
                "repeat_identical": first.read_bytes() == second.read_bytes(),
                "warnings": direct.warning_dicts(),
                "validation": validation.to_dict(),
            }
        )

    serial = pack_segmented(
        model,
        "v3",
        checkpoints,
        workers=1,
        parameter_version="demo-p1",
        artifact_path=output / "g01_segmented_serial.mfz",
        truth_path=truth_path(sample),
    )
    parallel = pack_segmented(
        model,
        "v3",
        checkpoints,
        workers=4,
        parameter_version="demo-p1",
        artifact_path=output / "g01_segmented_parallel.mfz",
        truth_path=truth_path(sample),
    )
    report["paths"]["serial"] = serial.to_dict()
    report["paths"]["parallel"] = parallel.to_dict()
    report["serial_parallel_identical"] = (
        serial.artifact_sha256 == parallel.artifact_sha256
        and (output / "g01_segmented_serial.mfz").read_bytes()
        == (output / "g01_segmented_parallel.mfz").read_bytes()
    )
    corrupt = pack_segmented(
        model,
        "v3",
        checkpoints,
        workers=2,
        parameter_version="demo-p1",
        injection=InjectionConfig(corrupt_track="audio_main", corrupt_offset=31, parameter_version="demo-p1"),
    )
    retry = pack_segmented(
        model,
        "v3",
        checkpoints,
        workers=2,
        parameter_version="demo-p1",
        artifact_path=output / "g01_segmented_retry.mfz",
        truth_path=truth_path(sample),
    )
    report["injection"] = corrupt.to_dict()
    report["retry"] = retry.to_dict()
    report["retry_matches_serial"] = (
        retry.artifact_sha256 == serial.artifact_sha256
        and (output / "g01_segmented_retry.mfz").read_bytes()
        == (output / "g01_segmented_serial.mfz").read_bytes()
    )
    anomaly_checks = []
    for name in ("conflict", "order", "missing_track", "corrupt_segment"):
        config = InjectionConfig.from_file(ROOT / "samples" / "injections" / f"{name}.json")
        try:
            load_sample(sample, "v3", injection=config)
            anomaly_checks.append({"name": name, "code": None})
        except WorkbenchError as exc:
            anomaly_checks.append(
                {
                    "name": name,
                    "code": exc.code,
                    "field_path": exc.field_path,
                    "message": exc.message,
                }
            )
    report["anomalies"] = anomaly_checks
    report["ok"] = (
        all(item["repeat_identical"] and item["validation"]["ok"] for item in report["exports"])
        and report["serial_parallel_identical"]
        and not report["injection"]["ok"]
        and report["injection"]["failures"][0]["code"] == "E_CORRUPT"
        and report["retry_matches_serial"]
        and [item["code"] for item in anomaly_checks] == ["E_CONFLICT", "E_ORDER", "E_TRACK", "E_CORRUPT"]
    )
    report["report_sha256"] = sha256_bytes(
        json.dumps(report, ensure_ascii=False, sort_keys=True).encode("utf-8")
    )
    (output / "validation_report.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True))
    return 0 if report["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
