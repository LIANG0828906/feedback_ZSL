from __future__ import annotations

import json
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from haptics.errors import ErrorCode
from haptics.sample import sample_capabilities, sample_events
from haptics.validation import load_and_validate, validate_capabilities, validate_events


class ValidationTests(unittest.TestCase):
    def setUp(self) -> None:
        self.capabilities = sample_capabilities()
        self.stream = sample_events()

    def test_sample_is_valid_and_frame_index_resets(self) -> None:
        cap_result = validate_capabilities(self.capabilities)
        event_result = validate_events(self.stream, self.capabilities)
        self.assertTrue(cap_result.ok, cap_result.issues)
        self.assertTrue(event_result.ok, event_result.issues)

    def test_frame_gap_returns_e_seq_with_event_and_field(self) -> None:
        events = list(self.stream.events)
        events[5] = replace(events[5], frame=events[5].frame + 1)
        broken = replace(self.stream, events=tuple(events))
        result = validate_events(broken, self.capabilities)
        issues = result.by_code(ErrorCode.E_SEQ)
        self.assertTrue(issues)
        self.assertEqual(issues[0].event_id, events[5].event_id)
        self.assertEqual(issues[0].field, "frame")

    def test_channel_out_of_range_returns_e_chan(self) -> None:
        events = list(self.stream.events)
        events[0] = replace(events[0], channel=9)
        result = validate_events(replace(self.stream, events=tuple(events)), self.capabilities)
        issues = result.by_code(ErrorCode.E_CHAN)
        self.assertTrue(issues)
        self.assertEqual(issues[0].event_id, events[0].event_id)
        self.assertEqual(issues[0].field, "channel")

    def test_invalid_intensity_returns_e_para(self) -> None:
        events = list(self.stream.events)
        events[0] = replace(events[0], intensity=1.5)
        result = validate_events(replace(self.stream, events=tuple(events)), self.capabilities)
        issues = result.by_code(ErrorCode.E_PARA)
        self.assertTrue(issues)
        self.assertEqual(issues[0].event_id, events[0].event_id)
        self.assertEqual(issues[0].field, "intensity")

    def test_invalid_capability_interval_returns_e_para(self) -> None:
        channel = replace(self.capabilities.channels[0], min_event_interval_ms=-1.0)
        capabilities = replace(
            self.capabilities, channels=(channel, self.capabilities.channels[1])
        )
        result = validate_capabilities(capabilities)
        issues = result.by_code(ErrorCode.E_PARA)
        self.assertTrue(issues)
        self.assertEqual(issues[0].event_id, "<capability>")
        self.assertEqual(issues[0].field, "min_event_interval_ms")

    def test_capability_version_mismatch_is_e_para(self) -> None:
        result = validate_events(replace(self.stream, capability_version=99), self.capabilities)
        issues = [item for item in result.by_code(ErrorCode.E_PARA) if item.field == "capability_version"]
        self.assertTrue(issues)

    def test_json_load_and_validate(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory)
            events_path = base / "events.json"
            caps_path = base / "caps.json"
            events_path.write_text(
                json.dumps(self.stream.to_dict(), ensure_ascii=False), encoding="utf-8"
            )
            caps_path.write_text(
                json.dumps(self.capabilities.to_dict(), ensure_ascii=False), encoding="utf-8"
            )
            stream, capabilities, result = load_and_validate(events_path, caps_path)
            self.assertTrue(result.ok, result.issues)
            self.assertIsNotNone(stream)
            self.assertIsNotNone(capabilities)

    def test_stream_may_start_at_nonzero_frame_without_gap(self) -> None:
        from haptics.models import EventStream, HapticEvent

        events = tuple(
            replace(event, frame=event.frame + 10)
            for event in self.stream.events
        )
        result = validate_events(
            EventStream(events=events, capability_version=self.capabilities.capability_version),
            self.capabilities,
        )
        self.assertTrue(result.ok, result.issues)

    def test_json_boolean_string_is_not_coerced(self) -> None:
        payload = self.stream.to_dict()
        payload["events"][0]["no_interrupt"] = "yes"
        result = validate_events(payload, self.capabilities)
        issues = [
            item
            for item in result.by_code(ErrorCode.E_PARA)
            if item.field == "no_interrupt"
        ]
        self.assertTrue(issues)
        self.assertEqual(issues[0].event_id, self.stream.events[0].event_id)

    def test_json_numeric_string_interval_is_e_para(self) -> None:
        payload = self.capabilities.to_dict()
        payload["channels"][0]["min_event_interval_ms"] = "32"
        result = validate_capabilities(payload)
        issues = [
            item
            for item in result.by_code(ErrorCode.E_PARA)
            if item.field == "min_event_interval_ms"
        ]
        self.assertTrue(issues)
        self.assertEqual(issues[0].channel, 0)


if __name__ == "__main__":
    unittest.main()

