from scoreworkbench.errors import DanglingError, ParameterError
from scoreworkbench.model import ExpressionEvent, Note
from scoreworkbench.session import EditSession
from scoreworkbench.versioning import load_score

from .base import SampleBackedTestCase


class SessionTests(SampleBackedTestCase):
    def setUp(self):
        self.project = load_score(self.v3)
        self.session = EditSession(self.project.score)

    def test_uncommitted_edits_do_not_affect_committed_model(self):
        first_note = self.session.working.notes[0]
        original_velocity = first_note.velocity
        self.session.edit_note(first_note.note_id, velocity=0.42)
        self.assertEqual(
            self.session.committed.note_by_id(first_note.note_id).velocity,
            original_velocity,
        )
        self.assertTrue(self.session.has_pending())

    def test_commit_returns_affected_object_set(self):
        note = self.session.working.notes[0]
        self.session.edit_note(note.note_id, pitch=80)
        record = self.session.commit()
        self.assertEqual(record.affected_objects, [note.note_id])
        self.assertTrue(record.committed)
        self.assertEqual(
            self.session.committed.note_by_id(note.note_id).pitch, 80
        )
        self.assertFalse(self.session.has_pending())

    def test_failed_batch_rolls_back_entire_model(self):
        note = self.session.working.notes[0]
        before = self.session.committed.clone()
        self.session.edit_note(note.note_id, velocity=0.5)
        event = self.session.working.events_for_note(note.note_id)[0]
        event.note_id = "ghost-note"
        with self.assertRaises(DanglingError):
            self.session.commit()
        self.assertEqual(
            self.session.committed.to_dict(), before.to_dict()
        )
        self.assertTrue(self.session.has_pending())
        self.session.rollback()
        self.assertFalse(self.session.has_pending())

    def test_note_move_migrates_bound_events(self):
        note = self.session.working.notes[0]
        bound = self.session.working.events_for_note(note.note_id)
        old_anchors = [(event.measure, event.anchor_beat) for event in bound]
        self.session.edit_note(
            note.note_id, measure=note.measure + 1,
            start_beat=note.start_beat + 0.5,
        )
        for event, (old_measure, old_anchor) in zip(bound, old_anchors):
            self.assertEqual(event.measure, old_measure + 1)
            self.assertAlmostEqual(event.anchor_beat, old_anchor + 0.5)

    def test_delete_note_removes_bound_events_without_dangling(self):
        note = self.session.working.notes[0]
        bound_ids = {
            event.event_id
            for event in self.session.working.events_for_note(note.note_id)
        }
        self.session.delete_note(note.note_id)
        remaining = {event.event_id for event in self.session.working.events}
        self.assertTrue(bound_ids.isdisjoint(remaining))
        self.session.commit()
        self.session.committed.validate_references()

    def test_adding_orphan_event_raises_e_dangle(self):
        orphan = ExpressionEvent(
            event_id="e-orphan", kind="velocity", note_id="nope",
            measure=0, anchor_beat=0.0, value=0.5,
        )
        with self.assertRaises(DanglingError) as catcher:
            self.session.add_event(orphan)
        self.assertEqual(catcher.exception.details["target_note_id"], "nope")

    def test_invalid_velocity_raises_e_para(self):
        note = self.session.working.notes[0]
        with self.assertRaises(ParameterError) as catcher:
            self.session.edit_note(note.note_id, velocity=1.8)
        self.assertEqual(catcher.exception.details["note_id"], note.note_id)

    def test_negative_beat_raises_e_para(self):
        note = self.session.working.notes[0]
        with self.assertRaises(ParameterError):
            self.session.edit_note(note.note_id, start_beat=-1.0)

    def test_velocity_curve_bulk_edit_is_atomic(self):
        velocity_events = [
            event for event in self.session.working.events
            if event.kind == "velocity"
        ]
        assignments = {event.event_id: 0.33 for event in velocity_events}
        assignments["missing-event"] = 0.5
        with self.assertRaises(ParameterError):
            self.session.set_velocity_curve(assignments)
        for event in velocity_events:
            self.assertNotEqual(
                self.session.working.event_by_id(event.event_id).value, 0.33
            )
        self.assertTrue(
            all(
                self.session.working.event_by_id(event.event_id).value != 0.33
                for event in velocity_events
            )
        )

    def test_rollback_discards_working_copy_changes(self):
        note = self.session.working.notes[0]
        original = note.pitch
        self.session.edit_note(note.note_id, pitch=original + 12)
        self.session.rollback()
        self.assertEqual(
            self.session.working.note_by_id(note.note_id).pitch, original
        )
        self.assertFalse(self.session.has_pending())

    def test_replace_note_migrates_bindings(self):
        note = self.session.working.notes[0]
        replacement = Note(
            note_id=note.note_id, measure=note.measure + 1,
            start_beat=note.start_beat, duration_beats=note.duration_beats,
            pitch=note.pitch, velocity=note.velocity,
            articulation=note.articulation,
            expression_mark=note.expression_mark,
        )
        self.session.replace_note(replacement)
        bound = self.session.working.events_for_note(note.note_id)
        self.assertTrue(bound)
        self.assertEqual(bound[0].measure, note.measure + 1)
