import json

from scoreworkbench.errors import (
    DanglingError,
    DecodeError,
    SchemaError,
    SemanticError,
)
from scoreworkbench.versioning import load_many, load_score

from .base import SampleBackedTestCase


class LoaderTests(SampleBackedTestCase):
    def test_loads_all_three_versions(self):
        for path in (self.v1, self.v2, self.v3):
            project = load_score(path)
            self.assertEqual(project.source_path, str(path))
            self.assertGreater(len(project.score.notes), 0)
            self.assertGreater(len(project.score.events), 0)
            self.assertTrue(
                any(trace.action == "normalise" for trace in project.score.traces)
            )

    def test_default_fills_are_traced(self):
        project = load_score(self.v1)
        fills = [t for t in project.score.traces if t.action == "default_fill"]
        self.assertTrue(fills)
        for trace in fills:
            self.assertTrue(trace.object_path.startswith("root."))
            self.assertIn("->", trace.detail)

    def test_unknown_version_raises_e_schema(self):
        with self.assertRaises(SchemaError) as catcher:
            load_score(self.broken / "unknown_version.json")
        details = catcher.exception.details
        self.assertEqual(details["version"], "9.9")
        self.assertTrue(details["field_path"].startswith("root."))
        self.assertIn("unknown_version.json", details["file_path"])

    def test_missing_required_field_raises_e_schema(self):
        with self.assertRaises(SchemaError) as catcher:
            load_score(self.broken / "missing_required.json")
        details = catcher.exception.details
        self.assertEqual(details["version"], "3.0")
        self.assertIn("pitch", details["field_path"])

    def test_corrupted_file_raises_e_decode(self):
        with self.assertRaises(DecodeError) as catcher:
            load_score(self.broken / "corrupted.json")
        self.assertIn("corrupted.json", catcher.exception.details["file_path"])

    def test_unreadable_file_raises_e_decode(self):
        with self.assertRaises(DecodeError):
            load_score(self.broken / "does-not-exist.json")

    def test_dangling_event_raises_e_dangle(self):
        with self.assertRaises(DanglingError) as catcher:
            load_score(self.broken / "dangling_event.json")
        details = catcher.exception.details
        self.assertEqual(details["target_note_id"], "n-does-not-exist")
        self.assertTrue(details["event_id"].startswith("e"))

    def test_semantic_conflict_raises_e_semantic(self):
        with self.assertRaises(SemanticError) as catcher:
            load_score(self.broken / "semantic_conflict.json")
        self.assertIn("timeMs", catcher.exception.message)
        self.assertIn("performanceEvents", catcher.exception.details["object_path"])

    def test_duplicate_identifier_raises_e_semantic(self):
        with self.assertRaises(SemanticError):
            load_score(self.broken / "duplicate_id.json")

    def test_parallel_batch_load_preserves_order(self):
        projects = load_many([self.v3, self.v1, self.v2])
        self.assertEqual(
            [project.source_version for project in projects],
            ["3.0", "1.0", "2.0"],
        )

    def test_absolute_time_is_version_independent(self):
        loaded = {
            project.source_version: project
            for project in load_many([self.v1, self.v2, self.v3])
        }
        reference = loaded["3.0"].score
        for version in ("1.0", "2.0"):
            score = loaded[version].score
            events_by_id = {event.event_id: event for event in score.events}
            for reference_event in reference.events:
                event = events_by_id.get(reference_event.event_id)
                if event is None:
                    continue
                ref_beat = reference.event_absolute_beat(reference_event)
                beat = score.event_absolute_beat(event)
                self.assertAlmostEqual(ref_beat, beat, places=6)
                self.assertAlmostEqual(
                    reference.beat_to_seconds(ref_beat),
                    score.beat_to_seconds(beat),
                    places=6,
                )

    def test_schema_version_key_absent(self):
        target = self.sample_root / "no_version.json"
        with open(self.v2, encoding="utf-8") as handle:
            document = json.load(handle)
        del document["schemaVersion"]
        with open(target, "w", encoding="utf-8") as handle:
            json.dump(document, handle)
        with self.assertRaises(SchemaError) as catcher:
            load_score(target)
        self.assertIsNone(catcher.exception.details["version"])
