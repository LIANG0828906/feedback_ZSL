from __future__ import annotations

import json
import os
import tempfile
import unittest
from pathlib import Path

import numpy as np

from volrender.buffers import BufferStore
from volrender.errors import ErrorCode, VolumeRenderError
from volrender.renderer import RenderResult, render_offscreen
from tests.helpers import make_volume


class OffscreenRendererTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._temp = tempfile.TemporaryDirectory()
        cls.volume = make_volume(Path(cls._temp.name), size=24)
        from volrender.synthetic import baseline_parameters

        cls.params = baseline_parameters(cls.volume, viewport=(56, 48)).with_changes(step_size=0.028)

    @classmethod
    def tearDownClass(cls) -> None:
        cls._temp.cleanup()

    def test_repeat_render_is_byte_identical(self) -> None:
        first = render_offscreen(self.volume, self.params)
        second = render_offscreen(self.volume, self.params)
        self.assertEqual(first.rgba_bytes, second.rgba_bytes)
        self.assertEqual(first.buffer_hash, second.buffer_hash)
        self.assertEqual(first.parameter_hash, self.params.parameter_hash)

    def test_parameter_change_changes_buffer(self) -> None:
        first = render_offscreen(self.volume, self.params)
        changed = self.params.with_changes(window_width=self.params.window_width * 1.4)
        second = render_offscreen(self.volume, changed)
        self.assertNotEqual(first.parameter_hash, second.parameter_hash)
        self.assertNotEqual(first.buffer_hash, second.buffer_hash)

    def test_mask_differences_are_confined_to_mask_rays(self) -> None:
        masked = render_offscreen(self.volume, self.params)
        without_mask_params = self.params.with_changes(mask_enabled=False, mask_hash="")
        without_mask = render_offscreen(self.volume, without_mask_params)
        self.assertIsNotNone(masked.mask_coverage)
        changed_pixels = np.any(masked.color != without_mask.color, axis=2)
        changed_outside = changed_pixels & ~masked.mask_coverage
        self.assertFalse(np.any(changed_outside))
        self.assertTrue(np.any(changed_pixels & masked.mask_coverage))

    def test_wrong_mask_hash_is_mask_error(self) -> None:
        bad = self.params.with_changes(mask_hash="b" * 64)
        with self.assertRaises(VolumeRenderError) as raised:
            render_offscreen(self.volume, bad)
        self.assertEqual(raised.exception.code, ErrorCode.E_MASK_INVALID)
        self.assertEqual(raised.exception.field, "mask_hash")

    def test_one_voxel_thickness_axes_render(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            from volrender.params import make_parameters
            from volrender.transfer import DEFAULT_CONTROL_POINTS
            from volrender.volume import load_volume, write_volume

            path = Path(directory) / "flat.vol"
            density = np.arange(8, dtype=np.uint16).reshape(1, 1, 8) * 100
            write_volume(path, density, (1.0, 1.0, 1.0), 400.0, 800.0)
            volume = load_volume(path)
            params = make_parameters(
                volume.content_sha256,
                400.0,
                800.0,
                DEFAULT_CONTROL_POINTS,
                step_size=0.05,
                viewport=(12, 12),
            )
            result = render_offscreen(volume, params)
            self.assertEqual(result.color.shape, (12, 12, 4))


class BufferStoreTests(unittest.TestCase):
    def test_publish_and_verified_load(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            volume = make_volume(Path(directory), size=20)
            from volrender.synthetic import baseline_parameters

            params = baseline_parameters(volume, viewport=(40, 40)).with_changes(step_size=0.04)
            result = render_offscreen(volume, params)
            store = BufferStore(Path(directory) / "buffers")
            stored = store.publish(result, params)
            loaded = store.load_verified(params.parameter_hash, expected_parameter_hash=params.parameter_hash)
            self.assertEqual(loaded.result.buffer_hash, result.buffer_hash)
            self.assertTrue(np.array_equal(loaded.result.color, result.color))
            os.utime(stored.pixels_path, None)
            loaded_again = store.load_verified(params.parameter_hash)
            self.assertEqual(loaded_again.result.buffer_hash, result.buffer_hash)

    def test_manifest_tampering_is_stale(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            volume = make_volume(Path(directory), size=20)
            from volrender.synthetic import baseline_parameters

            params = baseline_parameters(volume, viewport=(40, 40)).with_changes(step_size=0.04)
            result = render_offscreen(volume, params)
            store = BufferStore(Path(directory) / "buffers")
            stored = store.publish(result, params)
            manifest = json.loads(stored.manifest_path.read_text(encoding="utf-8"))
            manifest["parameter_hash"] = "c" * 64
            stored.manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaises(VolumeRenderError) as raised:
                store.load_verified(params.parameter_hash, expected_parameter_hash=params.parameter_hash)
            self.assertEqual(raised.exception.code, ErrorCode.E_PARAM_STALE)

    def test_manifest_missing_hash_is_stale(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            volume = make_volume(Path(directory), size=20)
            from volrender.synthetic import baseline_parameters

            params = baseline_parameters(volume, viewport=(40, 40)).with_changes(step_size=0.04)
            result = render_offscreen(volume, params)
            store = BufferStore(Path(directory) / "buffers")
            stored = store.publish(result, params)
            manifest = json.loads(stored.manifest_path.read_text(encoding="utf-8"))
            del manifest["parameter_hash"]
            stored.manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
            with self.assertRaises(VolumeRenderError) as raised:
                store.load_verified(params.parameter_hash)
            self.assertEqual(raised.exception.code, ErrorCode.E_PARAM_STALE)
            self.assertEqual(raised.exception.field, "parameter_hash")

    def test_result_without_hash_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            volume = make_volume(Path(directory), size=20)
            from volrender.synthetic import baseline_parameters

            params = baseline_parameters(volume, viewport=(32, 32)).with_changes(step_size=0.05)
            result = render_offscreen(volume, params)
            with self.assertRaises(VolumeRenderError) as raised:
                RenderResult(
                    color=result.color.copy(),
                    parameter_hash="",
                    buffer_hash=result.buffer_hash,
                    width=result.width,
                    height=result.height,
                )
            self.assertEqual(raised.exception.code, ErrorCode.E_PARAM_STALE)


if __name__ == "__main__":
    unittest.main()
