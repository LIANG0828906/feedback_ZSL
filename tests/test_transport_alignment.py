from __future__ import annotations

import unittest
from dataclasses import replace

from haptics.alignment import AlignmentReport
from haptics.codec import encode_stream
from haptics.errors import ErrorCode, HapticError
from haptics.sample import sample_capabilities, sample_events
from haptics.scheduler import schedule_serial
from haptics.transport import DispatchRecord, FaultPlan, LoopbackClient, MockDeviceService


def dispatch_records():
    caps = sample_capabilities()
    scheduled = schedule_serial(sample_events(), caps)
    encoded = encode_stream(scheduled, caps)
    records = [
        DispatchRecord(
            packet_seq=packet.packet_seq,
            channel=packet.channel,
            data=packet.data,
            sent_at_ms=packet.packet_seq * 2.0,
        )
        for packet in encoded
    ]
    return encoded, records


class TransportAlignmentTests(unittest.TestCase):
    def test_service_faults_are_classified_and_retry_is_isolated(self) -> None:
        encoded, records = dispatch_records()
        with MockDeviceService(fault_plan=FaultPlan.sample()) as service:
            client = LoopbackClient(service.host, service.port)
            echoes = client.send_batch("test-run", records, timeout_ms=30.0)
            report = AlignmentReport.build(encoded, echoes, timeout_ms=30.0)
            codes = {issue.code for issue in report.issues}
            self.assertIn(ErrorCode.E_MISS, codes)
            self.assertIn(ErrorCode.E_DUP, codes)
            self.assertIn(ErrorCode.E_TIMEOUT, codes)
            self.assertIn(ErrorCode.E_REORDER, codes)

            missing_seq = next(
                issue.packet_seq
                for issue in report.issues
                if issue.code == ErrorCode.E_MISS
            )
            untouched = {
                item.packet_seq: item.status
                for item in report.packets
                if item.packet_seq != missing_seq
            }
            record = next(item for item in records if item.packet_seq == missing_seq)
            echo = client.retry_packet("test-run", record, timeout_ms=30.0)
            retried = report.with_retry(missing_seq, echo)
            self.assertIsNone(retried.packet(missing_seq).issue)
            for seq, status in untouched.items():
                self.assertEqual(retried.packet(seq).status, status)
            self.assertEqual(retried.stats["TOTAL"], report.stats["TOTAL"])

    def test_local_packet_order_inversion_is_e_local(self) -> None:
        encoded, records = dispatch_records()
        with MockDeviceService() as service:
            client = LoopbackClient(service.host, service.port)
            echoes = client.send_batch("order-run", records, timeout_ms=30.0)
        inverted = list(encoded)
        inverted[0], inverted[1] = inverted[1], inverted[0]
        report = AlignmentReport.build(inverted, echoes, timeout_ms=30.0)
        self.assertFalse(report.local_order_ok)
        self.assertTrue(any(issue.code == ErrorCode.E_LOCAL for issue in report.issues))
        issue = next(issue for issue in report.issues if issue.code == ErrorCode.E_LOCAL)
        self.assertEqual(issue.field, "packet_seq")
        self.assertIsNotNone(issue.first_seen_ms)

    def test_loopback_port_conflict_returns_e_io(self) -> None:
        with MockDeviceService(port=0) as first:
            second = MockDeviceService(port=first.port)
            with self.assertRaises(HapticError) as context:
                second.start()
            self.assertEqual(context.exception.issue.code, ErrorCode.E_IO)
            self.assertEqual(context.exception.issue.field, "port")

    def test_missing_issue_reports_first_expected_time(self) -> None:
        encoded, records = dispatch_records()
        with MockDeviceService(
            fault_plan=FaultPlan(drops=(0,))
        ) as service:
            echoes = LoopbackClient(service.host, service.port).send_batch(
                "missing", records, timeout_ms=30.0
            )
        report = AlignmentReport.build(encoded, echoes, timeout_ms=30.0)
        issue = report.packet(0).issue
        self.assertEqual(issue.code, ErrorCode.E_MISS)
        self.assertEqual(issue.first_seen_ms, 0.0)


if __name__ == "__main__":
    unittest.main()

