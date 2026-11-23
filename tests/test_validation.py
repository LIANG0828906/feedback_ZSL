from __future__ import annotations

import json
import copy
import unittest

from gamepad_studio.errors import SampleValidationError
from gamepad_studio.validation import load_sample, validate_bundle

from tests.helpers import copy_sample, load_bundle, mutate_json


class ValidationTests(unittest.TestCase):
    def test_all_generated_samples_validate(self) -> None:
        for device_id in ("dual_stick", "single_stick", "hybrid_trigger_gyro"):
            bundle = load_bundle(device_id)
            self.assertEqual(validate_bundle(bundle), [], device_id)

    def test_missing_field_returns_schema_error_with_event(self) -> None:
        temporary, path = copy_sample()
        try:
            mutate_json(
                path / "events.json",
                lambda payload: payload["events"][0].pop("value"),
            )
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_SCHEMA")
            self.assertEqual(caught.exception.event_seq, 0)
            self.assertIn("value", caught.exception.field)
        finally:
            temporary.cleanup()

    def test_unknown_axis_returns_axis_error(self) -> None:
        temporary, path = copy_sample()
        try:
            def mutate(payload):
                payload["events"][0]["axis"] = "unknown_axis"

            mutate_json(path / "events.json", mutate)
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_AXIS")
            self.assertEqual(caught.exception.event_seq, 0)
            self.assertIn("axis", caught.exception.field)
        finally:
            temporary.cleanup()

    def test_axis_value_outside_declared_range_returns_axis_error(self) -> None:
        temporary, path = copy_sample()
        try:
            mutate_json(
                path / "events.json",
                lambda payload: payload["events"][0].update({"value": 9.0}),
            )
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_AXIS")
            self.assertEqual(caught.exception.event_seq, 0)
            self.assertIn("value", caught.exception.field)
        finally:
            temporary.cleanup()

    def test_timestamp_reversal_returns_order_error(self) -> None:
        temporary, path = copy_sample()
        try:
            mutate_json(
                path / "events.json",
                lambda payload: payload["events"][1].update(
                    {"t_ms": payload["events"][0]["t_ms"] - 1}
                ),
            )
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_ORDER")
            self.assertEqual(caught.exception.event_seq, 1)
            self.assertIn("t_ms", caught.exception.field)
        finally:
            temporary.cleanup()

    def test_duplicate_axis_id_returns_axis_error(self) -> None:
        temporary, path = copy_sample()
        try:
            mutate_json(
                path / "device.json",
                lambda payload: payload["axes"].append(copy.deepcopy(payload["axes"][0])),
            )
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_AXIS")
            self.assertIn("id", caught.exception.field)
        finally:
            temporary.cleanup()

    def test_unknown_button_event_returns_axis_error(self) -> None:
        temporary, path = copy_sample()
        try:
            events = json.loads((path / "events.json").read_text(encoding="utf-8"))
            button_index = next(
                index
                for index, event in enumerate(events["events"])
                if event["type"] == "button"
            )

            def mutate(payload):
                payload["events"][button_index]["button"] = "ghost"

            mutate_json(path / "events.json", mutate)
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_AXIS")
            self.assertEqual(caught.exception.event_seq, button_index)
        finally:
            temporary.cleanup()

    def test_time_window_out_of_range_returns_parameter_error(self) -> None:
        temporary, path = copy_sample()
        try:
            def mutate(payload):
                for rule in payload["rules"]:
                    if rule["kind"] == "combo":
                        rule["window_ms"] = 0
                        break

            mutate_json(path / "bindings.json", mutate)
            with self.assertRaises(SampleValidationError) as caught:
                load_sample(path)
            self.assertEqual(caught.exception.code, "E_PARA")
            self.assertIn("window_ms", caught.exception.field)
        finally:
            temporary.cleanup()


if __name__ == "__main__":
    unittest.main()

