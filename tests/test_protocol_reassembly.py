from __future__ import annotations

import unittest

from scworkbench.errors import CorruptError, SessionMismatchError
from scworkbench.loader import parse_session
from scworkbench.negotiation import negotiate
from scworkbench.profiles import CODEC_PROFILES
from scworkbench.protocol import MediaPacket, encode_frame_payload
from scworkbench.reassembly import FrameReassembler
from scworkbench.rendering import image_digest, render_scene

from tests.helpers import sample_data


class ProtocolTests(unittest.TestCase):
    def setUp(self) -> None:
        self.sample = parse_session(sample_data())
        self.negotiation = negotiate(self.sample)
        frame = self.sample.frames[0]
        self.image = render_scene(
            frame.scene,
            self.negotiation.width,
            self.negotiation.height,
            self.sample.declared_width,
            self.sample.declared_height,
            {"frame_seq": frame.seq},
        )
        self.encoded = encode_frame_payload(
            self.image,
            frame.seq,
            frame.timestamp_ms,
            self.sample.session_id,
            CODEC_PROFILES[self.negotiation.codec],
        )

    def _assembler(self) -> FrameReassembler:
        return FrameReassembler(
            self.sample.session_id,
            self.negotiation.width,
            self.negotiation.height,
        )

    def test_packet_roundtrip_and_fragment_count(self) -> None:
        self.assertGreater(len(self.encoded.packets), 1)
        packet = self.encoded.packets[3]
        decoded = MediaPacket.from_bytes(packet.to_bytes())
        self.assertEqual(decoded.frame_seq, packet.frame_seq)
        self.assertEqual(decoded.fragment_index, 3)
        self.assertEqual(decoded.fragment_count, len(self.encoded.packets))
        self.assertEqual(decoded.session_digest, packet.session_digest)

    def test_in_order_reassembly_matches_image_digest(self) -> None:
        assembler = self._assembler()
        for packet in self.encoded.packets:
            assembler.accept(packet)
        result = assembler.finalize()
        self.assertEqual(result.status, "complete")
        self.assertEqual(result.image_sha256, image_digest(self.image))
        self.assertEqual(result.codes, [])

    def test_out_of_order_fragments_still_reassemble_with_e_order(self) -> None:
        assembler = self._assembler()
        for packet in reversed(self.encoded.packets):
            assembler.accept(packet)
        result = assembler.finalize()
        self.assertEqual(result.status, "complete")
        self.assertEqual(result.image_sha256, image_digest(self.image))
        self.assertIn("E_ORDER", result.codes)

    def test_missing_fragment_is_e_loss_and_missing_marker(self) -> None:
        assembler = self._assembler()
        for packet in self.encoded.packets:
            if packet.fragment_index != 2:
                assembler.accept(packet)
        result = assembler.finalize()
        self.assertEqual(result.status, "missing")
        self.assertIn("E_LOSS", result.codes)
        self.assertEqual(result.missing_fragments, [2])

    def test_corrupt_payload_is_e_corrupt(self) -> None:
        wire = bytearray(self.encoded.packets[0].to_bytes())
        wire[-1] ^= 0xFF
        with self.assertRaises(CorruptError) as raised:
            MediaPacket.from_bytes(bytes(wire))
        self.assertEqual(raised.exception.code, "E_CORRUPT")

    def test_foreign_session_packet_is_e_session_and_does_not_pollute(self) -> None:
        assembler = self._assembler()
        with self.assertRaises(SessionMismatchError) as raised:
            assembler.accept(self.encoded.packets[0].with_foreign_session())
        self.assertEqual(raised.exception.code, "E_SESSION")
        for packet in self.encoded.packets:
            assembler.accept(packet)
        result = assembler.finalize()
        self.assertEqual(result.status, "complete")
        self.assertEqual(result.image_sha256, image_digest(self.image))


if __name__ == "__main__":
    unittest.main()
