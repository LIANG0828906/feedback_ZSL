from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import numpy as np

from vector_workbench.checkpoint import CheckpointStore
from vector_workbench.diff import compare_pipelines
from vector_workbench.errors import ErrorCode, WorkbenchError
from vector_workbench.model import RenderParams, Stage
from vector_workbench.operations import (
    load_sample_scene,
    verify_anomalies,
    verify_diff_detection,
    verify_snapshot_invalidation,
    verify_three_paths,
)
from vector_workbench.pipeline import Pipeline

ROOT = Path(__file__).resolve().parents[1]
DATA = ROOT / "data"


class PipelineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.scene = load_sample_scene(DATA, "badge_combo")

    def test_three_execution_paths_are_pixel_identical(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            report, pipelines = verify_three_paths(
                self.scene, RenderParams(), Path(directory)
            )
            self.assertTrue(report["passed"], json.dumps(report, indent=2))
            self.assertTrue(report["exact_pixel_equal"])
            self.assertTrue(report["commit_sequence_consistent"])
            self.assertEqual(
                report["full"]["image_sha256"],
                report["incremental"]["image_sha256"],
            )
            self.assertEqual(
                report["full"]["image_sha256"],
                report["resume"]["image_sha256"],
            )
            self.assertTrue(np.array_equal(
                pipelines["full"].canvas, pipelines["incremental"].canvas
            ))
            self.assertTrue(np.array_equal(
                pipelines["full"].canvas, pipelines["resume"].canvas
            ))
            self.assertTrue(report["incremental"]["prefix_recoverable"])
            self.assertFalse(report["resume"]["prefix_recoverable"])

    def test_parallel_and_sequential_workers_match(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            sequential = Pipeline(
                self.scene,
                RenderParams(max_parallelism=1),
                Path(directory) / "sequential",
            )
            parallel = Pipeline(
                self.scene,
                RenderParams(max_parallelism=4),
                Path(directory) / "parallel",
            )
            seq_result = sequential.run("full")
            par_result = parallel.run("full")
            self.assertEqual(seq_result.commit_sequence, par_result.commit_sequence)
            self.assertTrue(np.array_equal(sequential.canvas, parallel.canvas))
            self.assertTrue(compare_pipelines(sequential, parallel).equal)

    def test_parameter_injection_and_retry_are_isolated(self) -> None:
        path_id = self.scene.paths[0].id
        with tempfile.TemporaryDirectory() as directory:
            pipeline = Pipeline(
                self.scene,
                RenderParams(),
                directory,
                injections={
                    path_id: {"kind": "stage_failure", "stage": "coverage"}
                },
            )
            result = pipeline.run("full", raise_on_error=False)
            self.assertEqual(result.status, "failed")
            self.assertEqual(result.errors[0]["code"], "E_STAGE")
            row = next(item for item in pipeline.rows() if item["path_id"] == path_id)
            self.assertEqual(row["flatten"], "committed")
            self.assertEqual(row["coverage"], "failed")
            self.assertEqual(row["compose"], "pending")
            pipeline.set_injections({path_id: None})
            retry = pipeline.run_path(path_id, raise_on_error=False)
            self.assertEqual(retry.status, "partial")
            final = pipeline.run("resume", raise_on_error=False)
            self.assertEqual(final.status, "completed")
            self.assertTrue(
                all(
                    item[stage] == "committed"
                    for item in pipeline.rows()
                    for stage in ("flatten", "coverage", "compose")
                )
            )

    def test_anomaly_chains_and_stage_retry(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            report = verify_anomalies(DATA, directory)
            self.assertTrue(report["passed"], json.dumps(report, indent=2))
            codes = {
                case["scene_id"]: case["observed_codes"] for case in report["cases"]
            }
            self.assertEqual(codes["anomaly_self_intersection"], ["E_SELF"])
            self.assertEqual(codes["anomaly_degenerate"], ["E_DEGEN"])
            self.assertEqual(codes["anomaly_stroke_clip"], ["E_CLIP"])
            self.assertEqual(codes["anomaly_stage_failure"], ["E_STAGE"])
            stage_case = next(
                case
                for case in report["cases"]
                if case["scene_id"] == "anomaly_stage_failure"
            )
            self.assertEqual(stage_case["retry"]["status"], "completed")

    def test_diff_detection_reports_pixel_path_and_stage(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            report = verify_diff_detection(self.scene, RenderParams(), directory)
            self.assertTrue(report["passed"], json.dumps(report, indent=2))
            detected = report["detected_diff"]
            self.assertEqual(detected["first_pixel"], [0, 0])
            self.assertEqual(detected["stage"], "coverage")
            self.assertEqual(report["raised_code"], "E_DIFF")

    def test_snapshot_versions_and_corruption_invalidate_whole_snapshot(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            report = verify_snapshot_invalidation(
                self.scene, RenderParams(), Path(directory) / "versions"
            )
            self.assertTrue(report["passed"], json.dumps(report, indent=2))

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pipeline = Pipeline(self.scene, RenderParams(), root)
            first = pipeline.run("full")
            manifest = json.loads((root / "snapshot" / "manifest.json").read_text())
            self.assertEqual(
                manifest["param_hash"], RenderParams().fingerprint()
            )
            self.assertIn("scale", manifest["parameters"])
            self.assertIn("tolerance", manifest["parameters"])
            payload = root / "snapshot" / manifest["entries"][2]["file"]
            data = bytearray(payload.read_bytes())
            data[-1] ^= 0xFF
            payload.write_bytes(data)
            restarted = Pipeline(self.scene, RenderParams(), root)
            self.assertTrue(restarted.invalidated_snapshot)
            self.assertIn("payload", (restarted.invalidation_reason or "").lower())
            self.assertEqual(restarted.progress_committed, 0)
            second = restarted.run("incremental")
            self.assertEqual(first.commit_sequence, second.commit_sequence)
            self.assertTrue(np.array_equal(pipeline.canvas, restarted.canvas))

    def test_out_of_order_checkpoint_write_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            expected = [
                (path.id, stage)
                for path in self.scene.paths
                for stage in Stage.ordered()
            ]
            store = CheckpointStore(
                directory,
                scene_id=self.scene.scene_id,
                scene_hash=self.scene.canonical_hash(),
                param_hash=RenderParams().fingerprint(),
                algorithm_version="test-algorithm",
                expected_order=expected,
            )
            store.reset("test")
            store.commit(expected[0][0], Stage.FLATTEN, b"first")
            with self.assertRaises(WorkbenchError) as raised:
                store.commit(expected[5][0], Stage.FLATTEN, b"wrong")
            self.assertEqual(raised.exception.code, ErrorCode.E_STATE)
            self.assertEqual(store.commit_sequence(), [(expected[0][0], "flatten")])

    def test_out_of_range_parameters(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(WorkbenchError) as raised:
                Pipeline(
                    self.scene,
                    RenderParams(subsamples=0),
                    directory,
                )
            self.assertEqual(raised.exception.code, ErrorCode.E_PARA)
            self.assertEqual(raised.exception.field, "subsamples")


if __name__ == "__main__":
    unittest.main()

