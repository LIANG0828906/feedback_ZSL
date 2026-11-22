from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from tests.helpers import SAMPLES, load_sample_document, sample_path, write_json
from vp_pipeline.loader import load_scene, load_scene_result
from vp_pipeline.instructions import load_frame_instructions, validate_instructions
from vp_pipeline.renderer import render_all_frames
from vp_pipeline.errors import E_DECODE, E_SCHEMA, E_VER, PipelineError


class LoaderTests(unittest.TestCase):
    def test_all_versions_map_to_same_logical_scene(self) -> None:
        models = [load_scene(sample_path(index)) for index in (1, 2, 3)]
        for model in models:
            self.assertEqual(model.canvas.width, 40)
            self.assertEqual(model.canvas.height, 24)
            self.assertEqual(model.canvas.channels, 4)
            self.assertEqual(model.timeline.frames, 18)
            self.assertEqual(model.timeline.window_size, 3)
            self.assertEqual(
                (model.timeline.fps_num, model.timeline.fps_den), (24, 1)
            )
            self.assertEqual(
                [layer.layer_id for layer in model.layers],
                [
                    "bg_plate",
                    "prop_crate",
                    "prop_lamp",
                    "character",
                    "character_glow",
                ],
            )
            self.assertEqual(
                [layer.stack_order for layer in model.layers],
                [0, 1, 2, 3, 4],
            )
        self.assertEqual(models[0].source_version, "vpfc/1")
        self.assertEqual(models[1].source_version, "vpfc/2")
        self.assertEqual(models[2].source_version, "vpfc/3")
        self.assertEqual(models[1].layers[-1].parent_id, "character")
        self.assertEqual(models[2].layers[-1].parent_id, "character")
        self.assertIsNone(models[0].layers[-1].parent_id)

    def test_rendered_frames_are_equal_across_versions(self) -> None:
        rendered = [
            render_all_frames(load_scene(sample_path(index)))
            for index in (1, 2, 3)
        ]
        for frame_index in range(18):
            first = rendered[0][frame_index]
            for other in rendered[1:]:
                self.assertEqual(
                    first.pixel_bytes, other[frame_index].pixel_bytes
                )
                self.assertEqual(
                    first.content_hash, other[frame_index].content_hash
                )

    def test_sample_pixels_match_reference_truth(self) -> None:
        expected = json.loads(
            (SAMPLES / "truth" / "expected_frames.json").read_text(
                encoding="utf-8"
            )
        )
        for version in (1, 2, 3):
            frames = render_all_frames(load_scene(sample_path(version)))
            for expected_frame in expected["frames"]:
                frame = frames[expected_frame["frame"]]
                self.assertEqual(
                    list(frame.visible_layer_ids),
                    expected_frame["visible_layers"],
                )
                for sample in expected_frame["samples"]:
                    actual = frame.pixels[sample["y"], sample["x"]].tolist()
                    self.assertEqual(
                        actual,
                        sample["rgba8"],
                        f"version={version} frame={expected_frame['frame']} "
                        f"pixel=({sample['x']},{sample['y']})",
                    )

    def test_mapping_truth_tables_match(self) -> None:
        truth = json.loads(
            (SAMPLES / "truth" / "field_mappings.json").read_text(
                encoding="utf-8"
            )
        )
        for version in (1, 2, 3):
            result = load_scene_result(sample_path(version))
            self.assertEqual(
                list(result.mapping_trace),
                truth[f"vpfc/{version}"],
            )

    def test_frame_instructions_cover_all_frames_and_versions(self) -> None:
        instructions = load_frame_instructions(
            SAMPLES / "frame_instructions.json"
        )
        self.assertEqual(len(instructions.frames), 18)
        self.assertEqual(
            sum(len(frame.sample_pixels) for frame in instructions.frames), 108
        )
        for version in (1, 2, 3):
            validate_instructions(
                instructions, load_scene(sample_path(version))
            )

    def test_unknown_version_returns_e_ver(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            document = load_sample_document(1)
            document["format"]["version"] = "vpfc/99"
            path = Path(directory) / "scene.json"
            write_json(path, document)
            with self.assertRaises(PipelineError) as caught:
                load_scene(path)
            self.assertEqual(caught.exception.code, E_VER)
            self.assertEqual(caught.exception.field_path, "format.version")

    def test_missing_alpha_returns_e_schema_with_field_path(self) -> None:
        cases = []
        first = load_sample_document(1)
        first.pop("alpha_mode")
        cases.append((first, "alpha_mode"))
        second = load_sample_document(2)
        second["layers"][0]["rgba"].pop("alphaMode")
        cases.append((second, "layers[0].rgba.alphaMode"))
        third = load_sample_document(3)
        third["layers"][3]["children"][0]["alpha"].pop("mode")
        cases.append((third, "layers[3].children[0].alpha.mode"))
        with tempfile.TemporaryDirectory() as directory:
            for index, (document, expected_path) in enumerate(cases):
                path = Path(directory) / f"missing-{index}.json"
                write_json(path, document)
                with self.assertRaises(PipelineError) as caught:
                    load_scene(path)
                self.assertEqual(caught.exception.code, E_SCHEMA)
                self.assertEqual(caught.exception.field_path, expected_path)

    def test_undecidable_alpha_value_is_not_defaulted(self) -> None:
        document = load_sample_document(2)
        document["layers"][0]["rgba"]["alphaMode"] = "mystery"
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "scene.json"
            write_json(path, document)
            with self.assertRaises(PipelineError) as caught:
                load_scene(path)
        self.assertEqual(caught.exception.code, E_SCHEMA)
        self.assertEqual(
            caught.exception.field_path, "layers[0].rgba.alphaMode"
        )

    def test_inconsistent_premultiplied_rgb_is_rejected(self) -> None:
        document = load_sample_document(2)
        document["layers"][2]["rgba"]["values"] = [0.9, 0.2, 0.1, 0.72]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "scene.json"
            write_json(path, document)
            with self.assertRaises(PipelineError) as caught:
                load_scene(path)
        self.assertEqual(caught.exception.code, E_SCHEMA)
        self.assertEqual(
            caught.exception.field_path, "layers[2].rgba.values"
        )

    def test_corrupt_json_returns_e_decode(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "broken.json"
            path.write_text("{not valid json", encoding="utf-8")
            with self.assertRaises(PipelineError) as caught:
                load_scene(path)
            self.assertEqual(caught.exception.code, E_DECODE)


if __name__ == "__main__":
    unittest.main()
