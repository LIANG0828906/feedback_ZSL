from __future__ import annotations

import unittest

from poweraudit.ber import (
    TAG_GET_REQUEST,
    TAG_GET_RESPONSE,
    TAG_OCTET_STRING,
    TAG_SET_REQUEST,
    Varbind,
    decode_message,
    encode_message,
    oid_to_tuple,
)
from poweraudit.constants import DEVICE_ID, SYS_DESCR
from poweraudit.simulator import MibAgent


class ProtocolTests(unittest.TestCase):
    def test_get_round_trip(self) -> None:
        agent = MibAgent(
            {
                oid_to_tuple(SYS_DESCR): (TAG_OCTET_STRING, "sim"),
                oid_to_tuple(DEVICE_ID): (TAG_OCTET_STRING, "UPS-01"),
            },
            "public",
        )
        request = encode_message(
            "public", TAG_GET_REQUEST, 7, [SYS_DESCR, DEVICE_ID]
        )
        response = agent.handle(request, ("127.0.0.1", 1))
        self.assertIsNotNone(response)
        decoded = decode_message(response)
        self.assertEqual(decoded.pdu_tag, TAG_GET_RESPONSE)
        self.assertEqual(decoded.request_id, 7)
        self.assertEqual(decoded.varbinds[1].value, "UPS-01")

    def test_set_is_rejected_without_state_change(self) -> None:
        values = {oid_to_tuple(DEVICE_ID): (TAG_OCTET_STRING, "UPS-01")}
        agent = MibAgent(values, "public")
        set_packet = encode_message(
            "public",
            TAG_SET_REQUEST,
            8,
            (),
            varbinds=[Varbind(oid_to_tuple(DEVICE_ID), TAG_OCTET_STRING, "UPS-99")],
        )
        response = agent.handle(set_packet, ("127.0.0.1", 1))
        self.assertIsNotNone(response)
        decoded = decode_message(response)
        self.assertEqual(decoded.error_status, 17)
        self.assertEqual(agent.values[oid_to_tuple(DEVICE_ID)][1], "UPS-01")

    def test_wrong_community_is_ignored(self) -> None:
        agent = MibAgent(
            {oid_to_tuple(DEVICE_ID): (TAG_OCTET_STRING, "UPS-01")}, "private"
        )
        request = encode_message(
            "public", TAG_GET_REQUEST, 9, [DEVICE_ID]
        )
        self.assertIsNone(agent.handle(request, ("127.0.0.1", 1)))


if __name__ == "__main__":
    unittest.main()
