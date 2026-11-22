import json

from scoreworkbench.errors import LossyError, RoundtripError
from scoreworkbench.versioning import (
    compare_documents,
    export_score,
    load_score,
    roundtrip_check,
)

from .base import SampleBackedTestCase


class RoundtripTests(SampleBackedTestCase):
    def test_roundtrip_equivalent_for_every_version(self):
        for path in (self.v1, self.v2, self.v3):
            result = roundtrip_check(path)
            self.assertTrue(result["equivalent"])
            self.assertEqual(result["file_path"], str(path))

    def test_defaulted_optional_fields_are_not_reintroduced(self):
        project = load_score(self.v1)
        exported = export_score(
            project.score, "1.0", project.provenance, confirm_lossy=True
        )
        with open(self.v1, encoding="utf-8") as handle:
            original = json.load(handle)
        for original_note, exported_note in zip(
            original["notes"], exported["notes"]
        ):
            self.assertEqual(set(original_note), set(exported_note))

    def test_lossy_downgrade_requires_confirmation(self):
        project = load_score(self.v3)
        with self.assertRaises(LossyError) as catcher:
            export_score(project.score, "1.0", project.provenance)
        warnings = catcher.exception.details["warnings"]
        self.assertTrue(warnings)
        for warning in warnings:
            self.assertIn("field_path", warning)
            self.assertIn("object_id", warning)
        kinds = {item["reason"].split()[0] for item in warnings}
        self.assertTrue(kinds)

    def test_confirmed_downgrade_drops_only_unsupported_content(self):
        project = load_score(self.v3)
        exported = export_score(
            project.score, "1.0", project.provenance, confirm_lossy=True
        )
        self.assertEqual(exported["schema_version"], "1.0")
        self.assertNotIn(
            "pitch_bend",
            {item["event_type"] for item in exported["expression_events"]},
        )
        for note in exported["notes"]:
            self.assertNotIn("interactive_layer", note)
            self.assertNotEqual(note.get("articulation"), "accent")

    def test_v3_to_v2_lossy_warnings_identify_events(self):
        project = load_score(self.v3)
        with self.assertRaises(LossyError) as catcher:
            export_score(project.score, "2.0", project.provenance)
        warned_ids = {item["object_id"] for item in catcher.exception.details["warnings"]}
        pitch_events = {
            event.event_id for event in project.score.events
            if event.kind == "pitch_bend"
        }
        self.assertTrue(pitch_events.issubset(warned_ids))

    def test_comparator_reports_first_mismatch_path(self):
        mismatch = compare_documents(
            {"a": [1, 2, 3]}, {"a": [1, 2, 4]}
        )
        self.assertEqual(mismatch["path"], "$.a[2]")
        self.assertEqual(mismatch["expected"], 3)
        self.assertEqual(mismatch["actual"], 4)

    def test_roundtrip_error_on_tampered_export(self):
        project = load_score(self.v2)
        exported = {
            key: value
            for key, value in export_score(
                project.score, "2.0", project.provenance, confirm_lossy=True
            ).items()
            if not key.startswith("_")
        }
        with open(self.v2, encoding="utf-8") as handle:
            document = json.load(handle)
        # Simulate a corrupt exporter writing a wrong value on one field.
        original_value = exported["noteList"][0]["beatPos"]
        exported["noteList"][0]["beatPos"] = original_value + 1.5
        mismatch = compare_documents(document, exported)
        self.assertIsNotNone(mismatch)
        self.assertIn("beatPos", mismatch["path"])

    def test_roundtrip_check_wraps_mismatch_as_error(self):
        # A hand-crafted roundtrip runner that forces a mismatch must surface
        # E_ROUNDTRIP with the first mismatch path and both values.
        project = load_score(self.v2)
        exported = {
            key: value
            for key, value in export_score(
                project.score, "2.0", project.provenance, confirm_lossy=True
            ).items()
            if not key.startswith("_")
        }
        exported["noteList"][0]["beatLength"] = (
            exported["noteList"][0]["beatLength"] + 1.0
        )
        target = self.sample_root / "forced_mismatch_v2.json"
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(exported, handle)
        with self.assertRaises(RoundtripError) as catcher:
            roundtrip_check(target, reference_path=self.v2)
        mismatch = catcher.exception.details["first_mismatch"]
        self.assertIn("beatLength", mismatch["path"])
