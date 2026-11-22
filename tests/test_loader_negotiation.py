from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from scworkbench.errors import DecodeError, NegotiationError, SchemaError
from scworkbench.loader import load_session, parse_session
from scworkbench.negotiation import negotiate

from tests.helpers import sample_data, write_sample


class LoaderTests(unittest.TestCase):
    def setUp(self) -> None:
        self.session = parse_session(sample_data())

    def test_valid_sample_and_stable_content_hash(self) -> None:
        second = parse_session(sample_data())
        self.assertEqual(self.session.session_id, "cast-demo-uid-001")
        self.assertEqual(len(self.session.frames), 40)
        self.assertEqual(len(self.session.inputs), 28)
        self.assertEqual(self.session.content_hash, second.content_hash)
        self.assertEqual(self.session.sender.id, "game-pc-01")
        self.assertEqual(self.session.receiver.id, "workbench-01")

    def test_missing_required_field_is_e_schema(self) -> None:
        data = sample_data()
        data.pop("sender_device_id")
        with self.assertRaises(SchemaError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_SCHEMA")
        self.assertEqual(raised.exception.details["field"], "sender_device_id")

    def test_nonexistent_device_reference_is_e_schema(self) -> None:
        data = sample_data()
        data["receiver_device_id"] = "missing-device"
        with self.assertRaises(SchemaError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_SCHEMA")
        self.assertIn("不存在", raised.exception.message)

    def test_illegal_capability_value_is_e_schema(self) -> None:
        data = sample_data()
        data["devices"][0]["capabilities"]["codecs"] = ["vp9-unknown"]
        with self.assertRaises(SchemaError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_SCHEMA")

    def test_corrupt_scene_content_is_e_decode(self) -> None:
        data = sample_data()
        data["frames"][0]["scene"]["background"][0] = "not-a-color"
        with self.assertRaises(DecodeError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_DECODE")

    def test_non_monotonic_frame_sequence_is_e_decode(self) -> None:
        data = sample_data()
        data["frames"][2]["seq"] = 2
        with self.assertRaises(DecodeError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_DECODE")
        self.assertIn("帧序号", raised.exception.message)

    def test_unsorted_input_time_is_e_decode(self) -> None:
        data = sample_data()
        data["inputs"][3]["timestamp_ms"] = 1
        with self.assertRaises(DecodeError) as raised:
            parse_session(data)
        self.assertEqual(raised.exception.code, "E_DECODE")

    def test_malformed_json_is_e_decode(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "broken.json"
            path.write_text('{"schema_version": 1,', encoding="utf-8")
            with self.assertRaises(DecodeError) as raised:
                load_session(path)
            self.assertEqual(raised.exception.code, "E_DECODE")

    def test_write_and_load_file(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = write_sample(temp)
            loaded = load_session(path)
            self.assertEqual(loaded.content_hash, self.session.content_hash)


class NegotiationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.session = parse_session(sample_data())

    def test_best_common_codec_and_min_resolution(self) -> None:
        result = negotiate(self.session)
        self.assertEqual(result.codec, "hevc-main")
        self.assertEqual((result.width, result.height), (640, 360))
        self.assertEqual(result.fps, 12)
        self.assertTrue(result.supports_input)
        self.assertEqual(len(result.capabilities_hash), 64)

    def test_resolution_scales_down_to_intersection(self) -> None:
        data = sample_data()
        data["devices"][0]["capabilities"]["max_width"] = 320
        data["devices"][0]["capabilities"]["max_height"] = 180
        sample = parse_session(data)
        result = negotiate(sample)
        self.assertEqual((result.width, result.height), (320, 180))

    def test_no_common_codec_is_e_nego_with_items(self) -> None:
        data = sample_data()
        data["devices"][0]["capabilities"]["codecs"] = ["h264-baseline"]
        data["devices"][1]["capabilities"]["codecs"] = ["hevc-main"]
        sample = parse_session(data)
        with self.assertRaises(NegotiationError) as raised:
            negotiate(sample)
        self.assertEqual(raised.exception.code, "E_NEGO")
        self.assertIn("codecs", raised.exception.details["items"])

    def test_input_backchannel_requirement_is_e_nego(self) -> None:
        data = sample_data()
        data["devices"][1]["capabilities"]["supports_input"] = False
        sample = parse_session(data)
        with self.assertRaises(NegotiationError) as raised:
            negotiate(sample)
        self.assertEqual(raised.exception.code, "E_NEGO")
        self.assertIn("supports_input", raised.exception.details["items"])


if __name__ == "__main__":
    unittest.main()
