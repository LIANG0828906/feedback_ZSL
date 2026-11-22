"""工作台确定性与业务错误码测试（标准库 unittest，无外部测试依赖）。"""
from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest

import numpy as np

from segworkbench.core.errors import ErrorCode, WorkbenchError
from segworkbench.persistence.checkpoint import CheckpointStore
from segworkbench.pipeline.assemble import stitch_final_mask
from segworkbench.pipeline.blocks import all_block_ids, crop_block
from segworkbench.pipeline.engine import SegmentationEngine
from segworkbench.pipeline.growing import region_grow
from segworkbench.pipeline.loader import load_sample
from segworkbench.pipeline.morphology import binary_dilate, binary_erode
from segworkbench.pipeline.parameters import SegmentationParameters
from segworkbench.pipeline.samplegen import generate_all, generate_sample
from segworkbench.pipeline.states import COMMITTED, FAILED, PENDING, RUNNING, BlockStateMachine
from segworkbench.utils.hashing import sha256_array
from segworkbench.utils.pngcodec import decode_rgb, encode_rgb
from segworkbench.validation.verifier import run_verification

SAMPLE_ID = "sample_01_blocks"


class WorkbenchTestBase(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.mkdtemp(prefix="segwb-")
        self.data_dir = os.path.join(self.tmp, "data")
        self.ckpt_dir = os.path.join(self.tmp, "ckpt")
        generate_all(self.data_dir)

    def tearDown(self) -> None:
        shutil.rmtree(self.tmp, ignore_errors=True)

    def engine(self, params: SegmentationParameters | None = None,
               ckpt: str | None = None, block_ids=None) -> SegmentationEngine:
        sample = load_sample(self.data_dir, SAMPLE_ID)
        store = CheckpointStore(ckpt or self.ckpt_dir)
        return SegmentationEngine(sample, params or SegmentationParameters(), store, block_ids)


class TestDeterminism(WorkbenchTestBase):
    def test_png_round_trip(self) -> None:
        sample = load_sample(self.data_dir, SAMPLE_ID)
        blob = encode_rgb(sample.image)
        self.assertTrue(np.array_equal(decode_rgb(blob), sample.image))

    def test_sample_generator_is_deterministic(self) -> None:
        first = generate_sample(SAMPLE_ID, os.path.join(self.tmp, "a"), 1101)
        second = generate_sample(SAMPLE_ID, os.path.join(self.tmp, "b"), 1101)
        self.assertEqual(first["image"]["sha256"], second["image"]["sha256"])
        self.assertEqual(first["truth_fingerprint"], second["truth_fingerprint"])

    def test_mask_pixel_identical_across_runs(self) -> None:
        hashes = []
        for index in range(2):
            ckpt = os.path.join(self.tmp, f"run-{index}")
            engine = self.engine(ckpt=ckpt)
            engine.run_all()
            hashes.append(sha256_array(stitch_final_mask(CheckpointStore(ckpt))))
        self.assertEqual(hashes[0], hashes[1])

    def test_region_grow_order_independent(self) -> None:
        sample = load_sample(self.data_dir, SAMPLE_ID)
        block = crop_block(sample.image, "b00")
        params = SegmentationParameters()
        first = region_grow("b00", block, (0, 0), params,
                            sample.regions[0].truth)["raw_mask"]
        second = region_grow("b00", block, (0, 0), params,
                             sample.regions[0].truth)["raw_mask"]
        self.assertTrue(np.array_equal(first, second))

    def test_morphology_identity_at_radius_zero(self) -> None:
        mask = np.zeros((8, 8), dtype=bool)
        mask[2:5, 3:6] = True
        self.assertTrue(np.array_equal(binary_dilate(mask, 0), mask))
        self.assertTrue(np.array_equal(binary_erode(mask, 0), mask))


class TestErrorCodes(WorkbenchTestBase):
    def test_para_validation(self) -> None:
        for kwargs, field in (
            ({"color_tol": -1.0}, "color_tol"),
            ({"gradient_tol": 9999.0}, "gradient_tol"),
            ({"struct_radius": -2}, "struct_radius"),
            ({"min_region_pixels": 0}, "min_region_pixels"),
        ):
            with self.assertRaises(WorkbenchError) as captured:
                SegmentationParameters(**kwargs).validate()
            self.assertEqual(captured.exception.code, ErrorCode.E_PARA)
            self.assertEqual(captured.exception.context["field"], field)

    def test_seed_out_of_block_raises_para(self) -> None:
        sample = load_sample(self.data_dir, SAMPLE_ID)
        params = SegmentationParameters(seed_points={"b00": (999, 999)})
        with self.assertRaises(WorkbenchError) as captured:
            region_grow("b00", crop_block(sample.image, "b00"), (0, 0), params,
                        sample.regions[0].truth)
        self.assertEqual(captured.exception.code, ErrorCode.E_PARA)

    def test_missing_truth_region_via_input_file(self) -> None:
        target = os.path.join(self.tmp, "mutated-data")
        os.makedirs(target, exist_ok=True)
        manifest_path = os.path.join(self.data_dir, f"{SAMPLE_ID}.json")
        with open(manifest_path, "r", encoding="utf-8") as handle:
            manifest = json.load(handle)
        manifest["regions"] = [r for r in manifest["regions"] if r["block_id"] != "b02"]
        with open(os.path.join(target, f"{SAMPLE_ID}.json"), "w", encoding="utf-8") as handle:
            json.dump(manifest, handle)
        shutil.copy(os.path.join(self.data_dir, manifest["image"]["filename"]), target)
        with self.assertRaises(WorkbenchError) as captured:
            load_sample(target, SAMPLE_ID)
        self.assertEqual(captured.exception.code, ErrorCode.E_META)
        self.assertIn("b02", captured.exception.context["missing"])

    def test_decode_error_on_missing_manifest(self) -> None:
        os.remove(os.path.join(self.data_dir, f"{SAMPLE_ID}.json"))
        with self.assertRaises(WorkbenchError) as captured:
            load_sample(self.data_dir, SAMPLE_ID)
        self.assertEqual(captured.exception.code, ErrorCode.E_DECODE)

    def test_decode_error_on_corrupt_png(self) -> None:
        manifest = os.path.join(self.data_dir, f"{SAMPLE_ID}.json")
        with open(manifest, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        png_path = os.path.join(self.data_dir, payload["image"]["filename"])
        with open(png_path, "r+b") as handle:
            handle.seek(40)
            handle.write(b"\xff\xff\xff\xff")
        with self.assertRaises(WorkbenchError) as captured:
            load_sample(self.data_dir, SAMPLE_ID)
        self.assertEqual(captured.exception.code, ErrorCode.E_DECODE)

    def test_meta_error_on_channel_mismatch(self) -> None:
        manifest_path = os.path.join(self.data_dir, f"{SAMPLE_ID}.json")
        with open(manifest_path, "r", encoding="utf-8") as handle:
            payload = json.load(handle)
        payload["image"]["channels"] = 4
        with open(manifest_path, "w", encoding="utf-8") as handle:
            json.dump(payload, handle)
        with self.assertRaises(WorkbenchError) as captured:
            load_sample(self.data_dir, SAMPLE_ID)
        self.assertEqual(captured.exception.code, ErrorCode.E_META)
        self.assertEqual(captured.exception.context["field"], "image.channels")

    def test_seed_spill_region_block_injections(self) -> None:
        cases = [
            (SegmentationParameters(seed_points={"b00": (3, 3)},
                                    injections={"seed_background": {"blocks": ["b00"]}}),
             "b00", ErrorCode.E_SEED),
            (SegmentationParameters(color_tol=260.0, gradient_tol=400.0,
                                    injections={"spill_tolerance": {"blocks": ["b01"]}}),
             "b01", ErrorCode.E_SPILL),
            (SegmentationParameters(min_region_pixels=10 ** 9,
                                    injections={"region_missing": {"blocks": ["b02"]}}),
             "b02", ErrorCode.E_REGION),
            (SegmentationParameters(injections={"bad_block": {"blocks": ["b03"], "mode": "zero"}}),
             "b03", ErrorCode.E_BLOCK),
        ]
        for params, block_id, expected in cases:
            with self.subTest(code=expected):
                engine = self.engine(params=params,
                                     ckpt=os.path.join(self.tmp, f"inj-{expected}-{block_id}"),
                                     block_ids=[block_id])
                engine.initialize()
                with self.assertRaises(WorkbenchError) as captured:
                    engine.process_block(block_id)
                self.assertEqual(captured.exception.code, expected)
                self.assertEqual(engine.states.get(block_id, captured.exception.context["phase"]).state,
                                 FAILED)

    def test_failure_isolated_and_retryable(self) -> None:
        params = SegmentationParameters(injections={"bad_block": {"blocks": ["b00"], "mode": "zero"}})
        engine = self.engine(params=params,
                             ckpt=os.path.join(self.tmp, "retry"),
                             block_ids=["b00", "b01"])
        engine.initialize()
        with self.assertRaises(WorkbenchError) as captured:
            engine.process_block("b00")
        self.assertEqual(captured.exception.code, ErrorCode.E_BLOCK)
        # 其他块不受污染，可正常提交。
        engine.process_block("b01")
        self.assertEqual(engine.states.get("b01", "stats").state, COMMITTED)
        # 去掉注入后重试失败块：FAILED -> RUNNING -> COMMITTED。
        good_engine = self.engine(params=SegmentationParameters(),
                                  ckpt=os.path.join(self.tmp, "retry"),
                                  block_ids=["b00", "b01"])
        good_engine.process_block("b00")
        self.assertEqual(good_engine.states.get("b00", "stats").state, COMMITTED)


class TestStateMachine(unittest.TestCase):
    def test_legal_and_illegal_transitions(self) -> None:
        machine = BlockStateMachine(["b00"])
        self.assertEqual(machine.get("b00", "grow").state, PENDING)
        machine.move("b00", "grow", RUNNING)
        machine.move("b00", "grow", COMMITTED)
        with self.assertRaises(WorkbenchError) as captured:
            machine.move("b00", "grow", RUNNING)
        self.assertEqual(captured.exception.code, ErrorCode.E_STATE)
        self.assertEqual(machine.get("b00", "grow").state, COMMITTED)

    def test_committed_output_frozen(self) -> None:
        machine = BlockStateMachine(["b00"])
        machine.move("b00", "clean", RUNNING)
        machine.move("b00", "clean", COMMITTED)
        with self.assertRaises(WorkbenchError):
            machine.move("b00", "clean", FAILED)


class TestResumeAndVerification(WorkbenchTestBase):
    def test_resume_only_uses_persisted_state(self) -> None:
        ckpt = os.path.join(self.tmp, "resume")
        engine = self.engine(ckpt=ckpt)
        engine.initialize()
        engine.process_block("b00")
        engine.process_block("b01")
        rebuilt = self.engine(ckpt=ckpt)
        restored = rebuilt.restore()
        self.assertEqual(set(restored["b00"]), {"grow", "clean", "stats"})
        for block_id in all_block_ids():
            rebuilt.process_block(block_id)
        report = run_verification(ckpt, self.data_dir, SAMPLE_ID)
        self.assertTrue(report["passed"])

    def test_version_mismatch_invalidates_snapshot(self) -> None:
        ckpt = os.path.join(self.tmp, "version")
        engine = self.engine(ckpt=ckpt)
        engine.run_all()
        header_path = os.path.join(ckpt, "snapshot.json")
        with open(header_path, "r", encoding="utf-8") as handle:
            header = json.load(handle)
        header["algorithm_version"] = "alg-0.0.1"
        with open(header_path, "w", encoding="utf-8") as handle:
            json.dump(header, handle)
        rebuilt = self.engine(ckpt=ckpt)
        with self.assertRaises(WorkbenchError) as captured:
            rebuilt.restore()
        self.assertEqual(captured.exception.code, ErrorCode.E_VERSION)
        self.assertIsNone(CheckpointStore(ckpt).read_header())

    def test_verifier_detects_tampered_clean_mask(self) -> None:
        ckpt = os.path.join(self.tmp, "tamper")
        engine = self.engine(ckpt=ckpt)
        engine.run_all()
        target = os.path.join(ckpt, "blocks", "b02", "clean.npy")
        array = np.load(target)
        array[5, 5] ^= 1
        np.save(target, array)
        report = run_verification(ckpt, self.data_dir, SAMPLE_ID)
        self.assertFalse(report["passed"])
        first = report["first_mismatch"]
        self.assertEqual(first["block_id"], "b02")
        self.assertEqual(first["phase"], "clean")
        self.assertIn(first["kind"], {"clean_mask_pixel", "artifact_hash_break"})
        self.assertEqual(first["first_pixel_xy"], [5, 128 + 5])

    def test_verifier_detects_tampered_stats(self) -> None:
        ckpt = os.path.join(self.tmp, "tamper-stats")
        engine = self.engine(ckpt=ckpt)
        engine.run_all()
        target = os.path.join(ckpt, "blocks", "b00", "stats.json")
        with open(target, "r", encoding="utf-8") as handle:
            record = json.load(handle)
        record["metadata"]["statistics"]["area_pixels"] += 5
        record["metadata_sha256"] = "0" * 64
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(record, handle)
        with self.assertRaises(WorkbenchError) as captured:
            run_verification(ckpt, self.data_dir, SAMPLE_ID)
        self.assertEqual(captured.exception.code, ErrorCode.E_HASH)


class TestLowContrastSample(WorkbenchTestBase):
    def test_low_contrast_circles_within_tolerance(self) -> None:
        sample = load_sample(self.data_dir, "sample_02_circles")
        store = CheckpointStore(os.path.join(self.tmp, "low-contrast"))
        engine = SegmentationEngine(sample, SegmentationParameters(), store)
        engine.run_all()
        report = run_verification(store.root, self.data_dir, "sample_02_circles")
        self.assertTrue(report["passed"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
