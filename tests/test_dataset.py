from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

import numpy as np

from mshdr.dataset import DatasetIndex, load_scene
from mshdr.errors import WorkbenchError
from mshdr.injections import create_injected_dataset
from mshdr.io_utils import read_json, value_version, write_json

from support import generate_single_scene


class DatasetValidationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.root = Path(cls._temp.name)
        cls.index = generate_single_scene(cls.root)
        cls.scene_id = cls.index.scene_records[0]["scene_id"]
        cls.scene_root = cls.index.scene_root(cls.scene_id)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def _copy_dataset(self, name: str) -> tuple[Path, Path, str]:
        destination = self.root / name
        shutil.copytree(self.index.path.parent, destination)
        dataset_path = destination / self.index.path.name
        injected = DatasetIndex.load(dataset_path)
        return dataset_path, injected.scene_root(self.scene_id), self.scene_id

    def _reseal(self, dataset_path: Path, scene_root: Path) -> None:
        metadata_path = scene_root / "metadata.json"
        truth_path = scene_root / "truth.json"
        metadata = read_json(metadata_path, field="metadata")
        metadata.pop("metadata_version", None)
        version = value_version(metadata)
        metadata["metadata_version"] = version
        truth = read_json(truth_path, field="truth")
        truth["metadata_version"] = version
        write_json(metadata_path, metadata)
        write_json(truth_path, truth)
        dataset = read_json(dataset_path, field="dataset")
        for record in dataset["scenes"]:
            if record["scene_id"] == self.scene_id:
                record["metadata_version"] = version
        write_json(dataset_path, dataset)

    def test_generated_manifest_and_loaded_shapes(self) -> None:
        scene = self.index.load_scene(self.scene_id)
        self.assertGreaterEqual(scene.bands.shape[0], 3)
        self.assertEqual(scene.exposure_frames.shape[0], 3)
        self.assertEqual(scene.bands.shape[1:], scene.pixel_size)
        self.assertEqual(scene.detail_layer.shape, scene.pixel_size)
        self.assertEqual(scene.target_bit_depth, 10)
        for band in scene.metadata["bands"]:
            self.assertEqual(band["bit_depth"], 12)
            self.assertEqual(band["shape"], list(scene.pixel_size))

    def test_fixed_seed_generation_is_byte_deterministic(self) -> None:
        with tempfile.TemporaryDirectory() as first_dir, tempfile.TemporaryDirectory() as second_dir:
            first = generate_single_scene(Path(first_dir))
            second = generate_single_scene(Path(second_dir))
            first_root = first.path.parent
            second_root = second.path.parent
            for path in sorted(first_root.rglob("*")):
                if not path.is_file() or path.suffix == ".png":
                    continue
                relative = path.relative_to(first_root)
                self.assertEqual(path.read_bytes(), (second_root / relative).read_bytes(), relative.as_posix())

    def test_missing_metadata_reports_field(self) -> None:
        dataset_path, scene_root, _scene_id = self._copy_dataset("missing_meta")
        metadata_path = scene_root / "metadata.json"
        metadata = read_json(metadata_path, field="metadata")
        del metadata["pixel_size"]
        metadata.pop("metadata_version", None)
        metadata["metadata_version"] = value_version(metadata)
        write_json(metadata_path, metadata)
        self._reseal(dataset_path, scene_root)
        with self.assertRaises(WorkbenchError) as context:
            load_scene(scene_root)
        self.assertEqual(context.exception.code, "E_META")
        self.assertEqual(context.exception.field, "pixel_size")

    def test_band_count_mismatch_reports_bands(self) -> None:
        dataset_path, scene_root, _scene_id = self._copy_dataset("missing_band_meta")
        metadata = read_json(scene_root / "metadata.json", field="metadata")
        metadata["bands"] = metadata["bands"][:-1]
        write_json(scene_root / "metadata.json", metadata)
        self._reseal(dataset_path, scene_root)
        with self.assertRaises(WorkbenchError) as context:
            DatasetIndex.load(dataset_path).load_scene(self.scene_id)
        self.assertEqual(context.exception.code, "E_META")
        self.assertEqual(context.exception.field, "bands")

    def test_structural_meta_error_precedes_stale_version(self) -> None:
        dataset_path, scene_root, _scene_id = self._copy_dataset("stale_missing_band_meta")
        metadata = read_json(scene_root / "metadata.json", field="metadata")
        metadata["bands"] = metadata["bands"][:-1]
        write_json(scene_root / "metadata.json", metadata)
        with self.assertRaises(WorkbenchError) as context:
            DatasetIndex.load(dataset_path).load_scene(self.scene_id)
        self.assertEqual(context.exception.code, "E_META")
        self.assertEqual(context.exception.field, "bands")

    def test_missing_band_file_reports_meta(self) -> None:
        destination = self.root / "injected_missing_band"
        create_injected_dataset(
            self.index.path,
            destination,
            kind="missing_band",
            scene_id=self.scene_id,
            band_index=1,
        )
        with self.assertRaises(WorkbenchError) as context:
            DatasetIndex.load(destination / "dataset.json").load_scene(self.scene_id)
        self.assertEqual(context.exception.code, "E_META")
        self.assertIn("bands[1]", context.exception.field)

    def test_corrupted_pixel_payload_is_decode_error(self) -> None:
        _dataset_path, scene_root, _scene_id = self._copy_dataset("corrupt_payload")
        band = read_json(scene_root / "metadata.json", field="metadata")["bands"][0]
        band_path = scene_root / band["file"]
        with band_path.open("r+b") as handle:
            handle.seek(256)
            original = handle.read(4)
            handle.seek(256)
            handle.write(bytes(value ^ 0xFF for value in original))
        with self.assertRaises(WorkbenchError) as context:
            load_scene(scene_root)
        self.assertEqual(context.exception.code, "E_DECODE")
        self.assertIn("bands[0]", context.exception.field)

    def test_bit_depth_change_without_version_bump_is_version_error(self) -> None:
        destination = self.root / "injected_bit_depth"
        create_injected_dataset(self.index.path, destination, kind="bit_depth_change", scene_id=self.scene_id)
        with self.assertRaises(WorkbenchError) as context:
            DatasetIndex.load(destination / "dataset.json").load_scene(self.scene_id)
        self.assertEqual(context.exception.code, "E_VER")
        self.assertEqual(context.exception.field, "metadata_version")

    def test_exposure_order_injection_is_order_error(self) -> None:
        destination = self.root / "injected_order"
        create_injected_dataset(self.index.path, destination, kind="exposure_order", scene_id=self.scene_id)
        with self.assertRaises(WorkbenchError) as context:
            DatasetIndex.load(destination / "dataset.json").load_scene(self.scene_id)
        self.assertEqual(context.exception.code, "E_ORDER")
        self.assertIn("exposure_time", context.exception.field)

    def test_exposure_order_precedes_stale_version(self) -> None:
        _dataset_path, scene_root, _scene_id = self._copy_dataset("stale_exposure_order")
        metadata = read_json(scene_root / "metadata.json", field="metadata")
        frames = metadata["exposures"]["frames"]
        times = [frame["exposure_time"] for frame in frames]
        for frame, value in zip(frames, reversed(times), strict=True):
            frame["exposure_time"] = value
        write_json(scene_root / "metadata.json", metadata)
        with self.assertRaises(WorkbenchError) as context:
            load_scene(scene_root)
        self.assertEqual(context.exception.code, "E_ORDER")


if __name__ == "__main__":
    unittest.main()

