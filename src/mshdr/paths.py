"""Real-time, checkpoint-resume, and full-recompute path validation."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .comparison import compare_outputs
from .dataset import DatasetIndex
from .errors import WorkbenchError
from .io_utils import write_json
from .params import WorkbenchParams
from .pipeline import WorkbenchEngine


def run_three_paths(
    dataset_path: Path,
    *,
    output_root: Path,
    checkpoint_root: Path,
    scene_ids: list[str] | None = None,
    params: WorkbenchParams | None = None,
    resume_fraction: float = 0.34,
) -> dict[str, Any]:
    index = DatasetIndex.load(Path(dataset_path))
    selected = scene_ids or [str(record["scene_id"]) for record in index.scene_records]
    output_root = Path(output_root)
    checkpoint_root = Path(checkpoint_root)
    base_params = params or WorkbenchParams(max_workers=4)
    scene_reports: list[dict[str, Any]] = []

    for scene_id in selected:
        scene = index.load_scene(scene_id)
        full_dir = output_root / "full" / scene_id
        realtime_dir = output_root / "realtime" / scene_id
        resume_dir = output_root / "resume" / scene_id

        full_engine = WorkbenchEngine(
            scene,
            base_params.with_updates(max_workers=4),
            start_mode="none",
        )
        full_result = full_engine.run(output_dir=full_dir, mode="full", verbose=False)

        realtime_engine = WorkbenchEngine(
            scene,
            base_params.with_updates(max_workers=1),
            checkpoint_root=checkpoint_root / "realtime",
            start_mode="fresh",
        )
        realtime_result = realtime_engine.run(output_dir=realtime_dir, mode="realtime", verbose=False)

        resume_engine_first = WorkbenchEngine(
            scene,
            base_params.with_updates(max_workers=2),
            checkpoint_root=checkpoint_root / "resume",
            start_mode="fresh",
        )
        commit_limit = max(1, min(resume_engine_first.total_count - 1, int(resume_engine_first.total_count * resume_fraction)))
        resume_engine_first.run(commit_limit=commit_limit, mode="resume-pre", verbose=False)
        resume_engine = WorkbenchEngine(
            scene,
            base_params.with_updates(max_workers=3),
            checkpoint_root=checkpoint_root / "resume",
            start_mode="resume",
        )
        resume_result = resume_engine.run(output_dir=resume_dir, mode="resume", verbose=False)

        comparisons = {
            "full_vs_realtime": compare_outputs(
                full_dir,
                realtime_dir,
                report_path=output_root / "comparisons" / scene_id / "full_vs_realtime.json",
                raise_on_diverge=False,
            ),
            "full_vs_resume": compare_outputs(
                full_dir,
                resume_dir,
                report_path=output_root / "comparisons" / scene_id / "full_vs_resume.json",
                raise_on_diverge=False,
            ),
            "realtime_vs_resume": compare_outputs(
                realtime_dir,
                resume_dir,
                report_path=output_root / "comparisons" / scene_id / "realtime_vs_resume.json",
                raise_on_diverge=False,
            ),
        }
        if not all(report["ok"] for report in comparisons.values()):
            first = next(
                (name, report) for name, report in comparisons.items() if not report["ok"]
            )
            raise WorkbenchError(
                "E_DIVERGE",
                f"Three-path comparison failed for {scene_id}",
                field=(first[1].get("first_mismatch") or {}).get("field"),
                details={"scene_id": scene_id, "comparison": first[0], "report": first[1]},
            )
        scene_reports.append(
            {
                "scene_id": scene_id,
                "comparisons": {name: report["ok"] for name, report in comparisons.items()},
                "first_mismatch": {name: report["first_mismatch"] for name, report in comparisons.items()},
                "residual_std": full_result.stats["filtering_quality"]["residual_std"],
                "edge_fidelity": full_result.stats["filtering_quality"]["edge_fidelity"],
                "out_of_gamut_count": full_result.stats["gamut"]["out_of_gamut_count"],
                "outputs": {
                    "full": str(full_dir),
                    "realtime": str(realtime_dir),
                    "resume": str(resume_dir),
                },
                "progress": {
                    "realtime": f"{realtime_result.committed_count}/{realtime_result.total_count}",
                    "resume": f"{resume_result.committed_count}/{resume_result.total_count}",
                },
            }
        )

    report = {
        "ok": True,
        "dataset": str(Path(dataset_path)),
        "scenes": scene_reports,
        "paths": ["full", "realtime", "resume"],
        "comparison_result_file": str((output_root / "paths_report.json").resolve()),
    }
    write_json(output_root / "paths_report.json", report)
    return report

