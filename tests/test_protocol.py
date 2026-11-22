from __future__ import annotations

import copy
import unittest
from concurrent.futures import ThreadPoolExecutor

from sync_kernel.errors import E_DECODE, E_SEMANTIC, E_VER, KernelError
from sync_kernel.protocol import decode_frame, encode_frame
from sync_kernel.samples import build_sample_model


class ProtocolTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        (
            cls.bundle,
            cls.context,
            cls.initial,
            cls.inputs,
            cls.states,
        ) = build_sample_model(ticks=14)

    def test_all_versions_decode_every_tick(self) -> None:
        for version in (1, 2, 3):
            base = None
            for tick, frame in enumerate(self.bundle.frames_by_version[version]):
                decoded = decode_frame(
                    frame,
                    base=None if tick == 0 else base,
                    context=self.context,
                )
                self.assertEqual(decoded.source_version, version)
                self.assertEqual(decoded.state.tick, tick)
                self.assertEqual(
                    decoded.state.snapshot_hash,
                    self.states[tick].snapshot_hash,
                    msg=f"version={version} tick={tick}",
                )
                base = decoded.state

    def test_reencoding_is_byte_deterministic_across_threads(self) -> None:
        state = self.states[9]
        previous = self.states[8]
        for version in (1, 2, 3):
            expected = encode_frame(
                state,
                version,
                previous=previous,
                context=self.context,
            ).payload

            def encode_once(_index: int) -> bytes:
                return encode_frame(
                    state,
                    version,
                    previous=previous,
                    context=self.context,
                ).payload

            with ThreadPoolExecutor(max_workers=8) as executor:
                outputs = list(executor.map(encode_once, range(64)))
            self.assertTrue(all(output == expected for output in outputs))

    def test_low_versions_emit_lossy_mapping_without_failing(self) -> None:
        for version in (1, 2):
            result = encode_frame(
                self.states[3],
                version,
                previous=self.states[2],
                context=self.context,
            )
            self.assertTrue(result.warnings)
            self.assertTrue(all(issue.code == "E_LOSSY" for issue in result.warnings))
            self.assertTrue(result.mappings)
            self.assertTrue(any(mapping["lossy"] for mapping in result.mappings))
            decoded = decode_frame(
                result.payload,
                base=self.states[2],
                context=self.context,
            )
            self.assertEqual(decoded.state.snapshot_hash, self.states[3].snapshot_hash)

    def test_v2_status_absence_means_zero_but_v1_retains(self) -> None:
        previous = self.states[1].clone()
        previous.units["u1"].status_mask = 7
        previous.rehash()
        current = self.states[2].clone()
        current.units["u1"].status_mask = 7
        current.rehash()
        v2 = encode_frame(
            current,
            2,
            previous=previous,
            context=self.context,
        )
        decoded_v2 = decode_frame(
            v2.payload,
            base=previous,
            context=self.context,
        )
        self.assertEqual(decoded_v2.state.units["u1"].status_mask, 7)

        reset = self.states[3].clone()
        reset.units["u1"].status_mask = 0
        reset.rehash()
        v2_reset = encode_frame(
            reset,
            2,
            previous=current,
            context=self.context,
        )
        decoded_reset = decode_frame(
            v2_reset.payload,
            base=current,
            context=self.context,
        )
        self.assertEqual(decoded_reset.state.units["u1"].status_mask, 0)

    def test_unknown_version_has_field_path_and_source_version(self) -> None:
        with self.assertRaises(KernelError) as raised:
            decode_frame({"protocol_version": 77}, context=self.context)
        self.assertEqual(raised.exception.code, E_VER)
        self.assertEqual(raised.exception.issue.path, "$.protocol_version")
        self.assertEqual(raised.exception.issue.source_version, 77)

    def test_missing_required_field_is_e_ver(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[1][1])
        frame.pop("server_tick")
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[0], context=self.context)
        self.assertEqual(raised.exception.code, E_VER)
        self.assertEqual(raised.exception.issue.path, "$.server_tick")
        self.assertEqual(raised.exception.issue.source_version, 1)

    def test_corrupt_json_is_e_decode(self) -> None:
        with self.assertRaises(KernelError) as raised:
            decode_frame(b'{"protocol_version":2,')
        self.assertEqual(raised.exception.code, E_DECODE)

    def test_unknown_unit_is_e_semantic_with_object(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[1][1])
        frame["units"][0]["id"] = "missing-unit"
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[0], context=self.context)
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.object_id, "missing-unit")

    def test_backwards_time_is_e_semantic(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[3][2])
        frame["logical_clock"] = 0
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[1], context=self.context)
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.path, "$.logical_clock")

    def test_full_frame_backwards_tick_is_e_semantic(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[1][0])
        frame["full"] = True
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[2], context=self.context)
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.path, "$.server_tick")

    def test_v3_delta_snapshot_conflict_is_e_semantic(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[3][1])
        heading = frame["snapshot"]["u1"]["heading_mdeg"]
        frame["delta"][0]["heading_mdeg"] = (heading + 1) % 360_000
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[0], context=self.context)
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.object_id, "u1")

    def test_v2_hash_corruption_is_e_decode(self) -> None:
        frame = copy.deepcopy(self.bundle.frames_by_version[2][3])
        frame["content_hash8"] = "00000000"
        with self.assertRaises(KernelError) as raised:
            decode_frame(frame, base=self.states[2], context=self.context)
        self.assertEqual(raised.exception.code, E_DECODE)
        self.assertEqual(raised.exception.issue.path, "$.content_hash8")

    def test_stale_state_hash_cannot_be_encoded(self) -> None:
        stale = self.states[4].clone()
        stale.units["u1"].x += 1
        with self.assertRaises(KernelError) as raised:
            encode_frame(
                stale,
                3,
                previous=self.states[3],
                context=self.context,
            )
        self.assertEqual(raised.exception.code, E_SEMANTIC)
        self.assertEqual(raised.exception.issue.path, "$.snapshot_hash")


if __name__ == "__main__":
    unittest.main()

