from __future__ import annotations

import copy
import json
import shutil
import tempfile
import unittest
from pathlib import Path

from brecon.baseline import check_baseline
from brecon.canonical import compact_json, state_hash
from brecon.config import SampleConfig
from brecon.engine import ReplayEngine
from brecon.errors import BreconError
from brecon.generator import Profile, generate_workspace
from brecon.lock import RoomBusyLock
from brecon.recovery import load_snapshot_file, validate_snapshot
from brecon.verifier import verify_workspace

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ("p2_s12_low", "p4_s20_medium", "p8_s30_high")


class ReferenceArtifactTests(unittest.TestCase):
    def test_replay_is_byte_exact_with_reference_artifacts(self) -> None:
        for sample in SAMPLES:
            with self.subTest(sample=sample):
                sample_dir = ROOT / "artifacts" / "demo" / "samples" / sample
                engine = ReplayEngine.from_sample_dir(sample_dir).run()
                trace = "".join(
                    compact_json(entry) + "\n" for entry in engine.trace_entries
                ).encode("utf-8")
                latency = (
                    compact_json(engine.latency_payload()) + "\n"
                ).encode("utf-8")
                baseline = (
                    compact_json(engine.baseline_payload()) + "\n"
                ).encode("utf-8")
                self.assertEqual(trace, (sample_dir / "trace.jsonl").read_bytes())
                self.assertEqual(latency, (sample_dir / "latency.json").read_bytes())
                self.assertEqual(
                    baseline, (sample_dir / "baseline.json").read_bytes()
                )
                final_snapshot = json.loads(
                    (sample_dir / "snapshots" / "latest.json").read_text(
                        encoding="utf-8"
                    )
                )
                self.assertEqual(
                    compact_json(engine.snapshot_payload()),
                    compact_json(final_snapshot),
                )

    def test_intermediate_snapshots_are_byte_exact(self) -> None:
        for sample in SAMPLES:
            with self.subTest(sample=sample):
                source = ROOT / "artifacts" / "demo" / "samples" / sample
                with tempfile.TemporaryDirectory() as temporary:
                    destination = Path(temporary) / sample
                    destination.mkdir()
                    shutil.copy(source / "config.json", destination / "config.json")
                    shutil.copy(
                        source / "operations.json", destination / "operations.json"
                    )
                    ReplayEngine.from_sample_dir(
                        destination, write_snapshots=True
                    ).run()
                    for reference in sorted((source / "snapshots").glob("step_*.json")):
                        self.assertEqual(
                            reference.read_bytes(),
                            (destination / "snapshots" / reference.name).read_bytes(),
                            reference.name,
                        )

    def test_baseline_check_and_sync_convergence(self) -> None:
        for sample in SAMPLES:
            with self.subTest(sample=sample):
                engine = ReplayEngine.from_sample_dir(
                    ROOT / "artifacts" / "demo" / "samples" / sample
                ).run()
                report = check_baseline(engine)
                self.assertTrue(report["ok"])
                self.assertEqual(report["steps_checked"], len(engine.trace_entries))
                self.assertTrue(
                    all(
                        record["difference"] <= record["tolerance"]
                        and record["status"] == "ok"
                        for record in engine.sync_observations
                    )
                )
                self.assertEqual(engine.divergences, [])

    def test_midpoint_snapshot_recovery_does_not_reapply_claims(self) -> None:
        for sample in SAMPLES:
            with self.subTest(sample=sample):
                sample_dir = ROOT / "artifacts" / "demo" / "samples" / sample
                config = SampleConfig.load(sample_dir)
                full = ReplayEngine.from_sample_dir(sample_dir).run()
                midpoint = config.observation.sync_steps[
                    len(config.observation.sync_steps) // 2
                ]
                snapshot = load_snapshot_file(
                    config.snapshots_path / f"step_{midpoint:06d}.json", config
                )
                recovered = ReplayEngine.from_snapshot(
                    config,
                    ReplayEngine._load_operations(config),
                    snapshot,
                ).run()
                self.assertEqual(recovered.trace_entries, full.trace_entries)
                self.assertEqual(recovered.state, full.state)
                self.assertEqual(recovered.chain, full.chain)
                claims = [
                    (operation["player_id"], operation["client_seq"])
                    for entry in recovered.trace_entries
                    for operation in entry["effective_ops"]
                    if operation["op_type"] == "claim"
                ]
                self.assertEqual(len(claims), len(set(claims)))


class FailurePathTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sample_dir = ROOT / "artifacts" / "demo" / "samples" / "p2_s12_low"
        self.config = SampleConfig.load(self.sample_dir)

    def test_snapshot_checksum_and_chain_validation(self) -> None:
        snapshot = load_snapshot_file(
            self.config.snapshots_path / "step_000006.json", self.config
        )
        broken_checksum = copy.deepcopy(snapshot)
        broken_checksum["buffer_depth"] += 1
        with self.assertRaises(BreconError) as checksum_error:
            validate_snapshot(broken_checksum, self.config)
        self.assertEqual(checksum_error.exception.code, "E_RECOVERY_INVALID")

        broken_chain = copy.deepcopy(snapshot)
        broken_chain["chain"][3]["chain_hash"] = "0" * 64
        payload = dict(broken_chain)
        payload.pop("checksum")
        broken_chain["checksum"] = __import__(
            "brecon.canonical", fromlist=["sha256_hex"]
        ).sha256_hex(compact_json(payload))
        with self.assertRaises(BreconError) as chain_error:
            validate_snapshot(broken_chain, self.config)
        self.assertEqual(chain_error.exception.code, "E_RECOVERY_INVALID")
        self.assertEqual(chain_error.exception.step, 4)
        self.assertEqual(
            chain_error.exception.field_path, "chain[3].chain_hash"
        )

    def test_baseline_version_and_truncation_errors(self) -> None:
        baseline = json.loads(
            (self.sample_dir / "baseline.json").read_text(encoding="utf-8")
        )
        self.assertEqual(state_hash(baseline["steps"][0]["state"]),
                         baseline["steps"][0]["state_hash"])
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "baseline.json"
            changed = copy.deepcopy(baseline)
            changed["model_version"] = "other-model"
            path.write_text(compact_json(changed) + "\n", encoding="utf-8")
            engine = ReplayEngine.from_sample_dir(self.sample_dir).run()
            with self.assertRaises(BreconError) as version_error:
                check_baseline(engine, path)
            self.assertEqual(version_error.exception.code, "E_BASELINE_VERSION")

            changed = copy.deepcopy(baseline)
            changed["steps"] = changed["steps"][:-2]
            path.write_text(compact_json(changed) + "\n", encoding="utf-8")
            with self.assertRaises(BreconError) as truncated_error:
                check_baseline(engine, path)
            self.assertEqual(truncated_error.exception.code, "E_BASELINE_INVALID")

    def test_first_divergence_diagnostic_contains_context(self) -> None:
        engine = ReplayEngine.from_sample_dir(self.sample_dir).run()
        baseline = json.loads(
            (self.sample_dir / "baseline.json").read_text(encoding="utf-8")
        )
        baseline["steps"][4]["state"]["inventory"][0]["items"][0]["quantity"] = 999
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "baseline.json"
            path.write_text(compact_json(baseline) + "\n", encoding="utf-8")
            report = check_baseline(engine, path)
        self.assertFalse(report["ok"])
        self.assertEqual(report["step"], 5)
        self.assertEqual(report["code"], "E_STATE_DIVERGENCE")
        self.assertIn("inventory", report["field_path"])
        self.assertIn("effective_ops", report)
        self.assertIn("buffer_before", report)

    def test_room_lock_returns_busy_until_release(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / ".replay.lock"
            first = RoomBusyLock("room-test", path).acquire()
            with self.assertRaises(BreconError) as busy:
                RoomBusyLock("room-test", path).acquire()
            self.assertEqual(busy.exception.code, "E_BUSY")
            first.release()
            second = RoomBusyLock("room-test", path).acquire()
            second.release()

    def test_explicit_non_one_observation_start(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "sample"
            destination.mkdir()
            shutil.copy(self.sample_dir / "config.json", destination / "config.json")
            shutil.copy(
                self.sample_dir / "operations.json", destination / "operations.json"
            )
            config = json.loads(
                (destination / "config.json").read_text(encoding="utf-8")
            )
            config["observation"]["start_step"] = 3
            config["observation"]["sync_steps"] = [3, 6, 9, 12]
            (destination / "config.json").write_text(
                compact_json(config) + "\n", encoding="utf-8"
            )
            engine = ReplayEngine.from_sample_dir(
                destination, write_snapshots=True
            ).run()
            self.assertEqual(
                [entry["step"] for entry in engine.trace_entries],
                list(range(3, 13)),
            )
            engine.write_outputs(include_baseline=True)
            self.assertTrue(check_baseline(engine)["ok"])
            snapshot = load_snapshot_file(
                destination / "snapshots" / "step_000006.json",
                SampleConfig.load(destination),
            )
            self.assertEqual(snapshot["chain"][0]["step"], 3)

    def test_latency_divergence_reports_window_and_source(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "sample"
            destination.mkdir()
            shutil.copy(self.sample_dir / "config.json", destination / "config.json")
            shutil.copy(
                self.sample_dir / "operations.json", destination / "operations.json"
            )
            config = json.loads(
                (destination / "config.json").read_text(encoding="utf-8")
            )
            config["observation"]["tolerance"] = -1
            (destination / "config.json").write_text(
                compact_json(config) + "\n", encoding="utf-8"
            )
            engine = ReplayEngine.from_sample_dir(destination).run()
            self.assertTrue(engine.divergences)
            divergence = engine.divergences[0]
            self.assertEqual(divergence["code"], "E_LATENCY_DIVERGE")
            self.assertIn("window_start_step", divergence)
            self.assertIn("window_end_step", divergence)
            self.assertIn("source", divergence)
            self.assertIn(
                "E_LATENCY_DIVERGE",
                {record["code"] for record in engine.runtime_rejections},
            )


class GeneratorTests(unittest.TestCase):
    def test_generator_is_deterministic_and_verifiable(self) -> None:
        profiles = [
            Profile(players=2, steps=10, disorder="low", seed=42, name="det_low"),
            Profile(players=4, steps=12, disorder="high", seed=43, name="det_high"),
        ]
        with tempfile.TemporaryDirectory() as first_directory, tempfile.TemporaryDirectory() as second_directory:
            generate_workspace(first_directory, profiles, force=True)
            generate_workspace(second_directory, profiles, force=True)
            first_root = Path(first_directory)
            second_root = Path(second_directory)
            for relative in (
                Path("active.json"),
                Path("samples/det_low/config.json"),
                Path("samples/det_low/operations.json"),
                Path("samples/det_low/baseline.json"),
                Path("samples/det_low/trace.jsonl"),
                Path("samples/det_low/latency.json"),
                Path("samples/det_high/config.json"),
                Path("samples/det_high/operations.json"),
                Path("samples/det_high/baseline.json"),
                Path("samples/det_high/trace.jsonl"),
            ):
                self.assertEqual(
                    (first_root / relative).read_bytes(),
                    (second_root / relative).read_bytes(),
                    str(relative),
                )
            report = verify_workspace(first_directory, write=True)
            self.assertTrue(report["ok"])
            codes = set()
            for sample in profiles:
                engine = ReplayEngine.from_sample_dir(
                    first_root / "samples" / sample.sample_name
                ).run()
                codes.update(record["code"] for record in engine.runtime_rejections)
            self.assertIn("E_OP_LATE", codes)
            self.assertIn("E_REVOKE_CLOSED", codes)
            if any(sample.disorder == "high" for sample in profiles):
                self.assertIn("E_CREDIT_CONFLICT", codes)


class GuiSmokeTests(unittest.TestCase):
    def test_gui_offscreen_auto_run(self) -> None:
        try:
            import PySide6  # noqa: F401
        except ImportError:
            self.skipTest("PySide6 is not installed in this interpreter")
        from brecon.gui import launch_gui
        import os

        previous = os.environ.get("QT_QPA_PLATFORM")
        os.environ["QT_QPA_PLATFORM"] = "offscreen"
        try:
            with tempfile.TemporaryDirectory() as temporary:
                screenshot = Path(temporary) / "gui.png"
                code = launch_gui(
                    workspace=ROOT / "artifacts" / "demo",
                    sample="p2_s12_low",
                    offscreen=True,
                    auto_run=True,
                    screenshot=str(screenshot),
                )
                self.assertEqual(code, 0)
                self.assertTrue(screenshot.is_file())
                self.assertGreater(screenshot.stat().st_size, 1000)
        finally:
            if previous is None:
                os.environ.pop("QT_QPA_PLATFORM", None)
            else:
                os.environ["QT_QPA_PLATFORM"] = previous


if __name__ == "__main__":
    unittest.main()

