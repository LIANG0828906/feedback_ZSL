import json
import os
import tempfile
import threading
from pathlib import Path

from scoreworkbench.constants import (
    CONVERSION_RULES_VERSION,
    DOMAIN_MODEL_VERSION,
)
from scoreworkbench.errors import SnapshotError
from scoreworkbench.persistence import SnapshotStore, open_or_reload
from scoreworkbench.versioning import load_score

from .base import SampleBackedTestCase


class PersistenceTests(SampleBackedTestCase):
    def setUp(self):
        self.snapshot_dir = tempfile.mkdtemp(prefix="scorebench-snap-")
        self.store = SnapshotStore(self.snapshot_dir)
        self.project = load_score(self.v3)

    def test_save_and_reload_roundtrip(self):
        self.project.score.traces  # ensure traces present
        self.store.save(self.project.score)
        bundle = self.store.load()
        self.assertEqual(bundle.score.to_dict(), self.project.score.to_dict())
        self.assertEqual(bundle.source_version, "3.0")
        self.assertEqual(bundle.source_path, str(self.v3))

    def test_snapshot_contains_version_identifiers(self):
        self.store.save(self.project.score)
        with open(self.store.snapshot_path, encoding="utf-8") as handle:
            envelope = json.load(handle)
        self.assertEqual(
            envelope["domain_model_version"], DOMAIN_MODEL_VERSION
        )
        self.assertEqual(
            envelope["conversion_rules_version"], CONVERSION_RULES_VERSION
        )

    def test_stale_domain_version_invalidates_whole_snapshot(self):
        self.store.save(self.project.score)
        with open(self.store.snapshot_path, encoding="utf-8") as handle:
            envelope = json.load(handle)
        envelope["domain_model_version"] = "0.9"
        with open(self.store.snapshot_path, "w", encoding="utf-8") as handle:
            json.dump(envelope, handle)
        with self.assertRaises(SnapshotError):
            self.store.load()
        self.assertFalse(self.store.snapshot_path.exists())

    def test_stale_conversion_rules_invalidate_snapshot(self):
        self.store.save(self.project.score)
        with open(self.store.snapshot_path, encoding="utf-8") as handle:
            envelope = json.load(handle)
        envelope["conversion_rules_version"] = "0"
        with open(self.store.snapshot_path, "w", encoding="utf-8") as handle:
            json.dump(envelope, handle)
        with self.assertRaises(SnapshotError):
            self.store.load()

    def test_torn_snapshot_is_rejected(self):
        self.store.save(self.project.score)
        with open(self.store.snapshot_path, "r+", encoding="utf-8") as handle:
            handle.truncate(20)
        with self.assertRaises(SnapshotError):
            self.store.load()

    def test_atomic_write_leaves_no_temp_files(self):
        self.store.save(self.project.score)
        leftovers = [
            name for name in os.listdir(self.snapshot_dir)
            if name.startswith(".snapshot-")
        ]
        self.assertEqual(leftovers, [])

    def test_serial_writes_keep_stable_content(self):
        errors = []

        def writer(index):
            try:
                score = self.project.score.clone()
                score.title = f"Cue {index}"
                self.store.save(score)
            except Exception as exc:  # pragma: no cover - failure path
                errors.append(exc)

        threads = [threading.Thread(target=writer, args=(i,)) for i in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertFalse(errors)
        bundle = self.store.load()
        self.assertTrue(bundle.score.title.startswith("Cue "))

    def test_open_or_reload_rebuilds_after_invalidation(self):
        call_counter = {"count": 0}

        def reload_callable():
            call_counter["count"] += 1
            return load_score(self.v3)

        bundle = open_or_reload(self.store, reload_callable)
        self.assertEqual(call_counter["count"], 1)
        self.store.invalidate()
        bundle_again = open_or_reload(self.store, reload_callable)
        self.assertEqual(call_counter["count"], 2)
        self.assertEqual(
            bundle_again.score.title, bundle.score.title
        )

    def test_batch_records_are_persisted(self):
        from scoreworkbench.session import EditSession

        session = EditSession(self.project.score)
        note = session.working.notes[0]
        session.edit_note(note.note_id, pitch=99)
        session.commit()
        self.store.save(session.committed)
        bundle = self.store.load()
        self.assertTrue(bundle.score.batch_records)
        self.assertIn(
            note.note_id, bundle.score.batch_records[-1].affected_objects
        )
