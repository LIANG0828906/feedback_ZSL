from __future__ import annotations

import json
import unittest

from tactical_pick.canonical import canonical_json_bytes
from tactical_pick.errors import E_TRACE_INVALID, PickError
from tactical_pick.generator import build_dataset
from tactical_pick.trace import (
    GENESIS_HASH,
    load_trace_bytes,
    replay_trace,
)


class TraceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.bundle = build_dataset(7)
        cls.run_output = cls.bundle.runs[0]
        cls.trace_bytes = cls.run_output.trace_bytes

    def test_trace_chain_and_exact_replay(self) -> None:
        trace = load_trace_bytes(self.trace_bytes)
        self.assertEqual(len(trace.records), len(self.run_output.recorder.records))
        self.assertEqual(trace.chain_head, self.run_output.recorder.chain_head)
        self.assertNotEqual(trace.chain_head, GENESIS_HASH)
        for record in trace.records:
            self.assertIn("camera", record)
            self.assertIn("region_version", record)
            self.assertIn("region_hash", record)
            self.assertNotIn("timestamp", record)
        replay = replay_trace(trace, self.run_output.store)
        self.assertEqual(replay["replayed_records"], len(trace.records))
        self.assertEqual(trace.to_bytes(), self.trace_bytes)

    def test_missing_footer_is_truncation(self) -> None:
        lines = self.trace_bytes.splitlines(keepends=True)
        truncated = b"".join(lines[:-1])
        with self.assertRaises(PickError) as caught:
            load_trace_bytes(truncated)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)
        self.assertEqual(caught.exception.location["field"], "trace.footer")

    def test_missing_final_newline_is_truncation(self) -> None:
        with self.assertRaises(PickError) as caught:
            load_trace_bytes(self.trace_bytes[:-1])
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)
        self.assertEqual(caught.exception.location["field"], "trace.eof")

    def test_broken_hash_chain_is_rejected(self) -> None:
        lines = self.trace_bytes.splitlines()
        record = json.loads(lines[1])
        record["hit"]["entity_id"] = 999
        lines[1] = canonical_json_bytes(record)
        broken = b"\n".join(lines) + b"\n"
        with self.assertRaises(PickError) as caught:
            load_trace_bytes(broken)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)
        self.assertIn("entry_hash", caught.exception.location["field"])

    def test_missing_version_is_trace_invalid(self) -> None:
        lines = self.trace_bytes.splitlines()
        record = json.loads(lines[1])
        del record["region_version"]
        lines[1] = canonical_json_bytes(record)
        broken = b"\n".join(lines) + b"\n"
        with self.assertRaises(PickError) as caught:
            load_trace_bytes(broken)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)
        self.assertIn("region_version", caught.exception.location["field"])

    def test_tampered_hit_replay_is_rejected_even_with_valid_chain(self) -> None:
        trace = load_trace_bytes(self.trace_bytes)
        records = [dict(record) for record in trace.records]
        records[0] = dict(records[0])
        records[0]["hit"] = dict(records[0]["hit"])
        records[0]["hit"]["entity_id"] = 12345
        # Rebuilding the whole chain makes it structurally valid but
        # inconsistent with the historical scene.
        previous = GENESIS_HASH
        rebuilt = []
        for index, record in enumerate(records):
            record["seq"] = index
            record["prev_hash"] = previous
            unhashed = {key: value for key, value in record.items() if key != "entry_hash"}
            from tactical_pick.canonical import sha256_json

            record["entry_hash"] = sha256_json(unhashed)
            previous = record["entry_hash"]
            rebuilt.append(record)
        header = json.loads(self.trace_bytes.splitlines()[0])
        footer = {
            "kind": "footer",
            "record_count": len(rebuilt),
            "chain_head": previous,
        }
        forged = b"".join(
            [
                canonical_json_bytes(header) + b"\n",
                *[canonical_json_bytes(record) + b"\n" for record in rebuilt],
                canonical_json_bytes(footer) + b"\n",
            ]
        )
        loaded = load_trace_bytes(forged)
        with self.assertRaises(PickError) as caught:
            replay_trace(loaded, self.run_output.store)
        self.assertEqual(caught.exception.code, E_TRACE_INVALID)
        self.assertIn("hit", caught.exception.location["field"])


if __name__ == "__main__":
    unittest.main()
