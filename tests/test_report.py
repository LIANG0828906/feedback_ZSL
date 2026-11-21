import json
import os

from syncbench.errors import E_REPORT_MISMATCH, SyncError
from syncbench.report import build_report, load_report, save_report

from .base import SampleSuiteTestCase


def _sample_report():
    return build_report(
        dataset={"dir": "skew4", "hash": "abc", "channels": 4, "fs": 48000},
        config={"seg_len_s": 2.0},
        results=[
            {"channel": 0, "fit": {"ppm": 0.0}},
            {"channel": 1, "fit": {"ppm": 40.0}},
        ],
    )


class TestReport(SampleSuiteTestCase):
    def test_roundtrip(self):
        report = _sample_report()
        path = os.path.join(self.out_dir("rep1"), "report.json")
        save_report(report, path)
        loaded = load_report(path)
        self.assertEqual(loaded, report)

    def test_atomic_no_tmp_left(self):
        path = os.path.join(self.out_dir("rep2"), "report.json")
        save_report(_sample_report(), path)
        self.assertFalse(os.path.exists(path + ".tmp"))

    def test_tampered_record_rejected(self):
        report = _sample_report()
        path = os.path.join(self.out_dir("rep3"), "report.json")
        save_report(report, path)
        with open(path) as fh:
            data = json.load(fh)
        data["records"][0]["data"]["fit"]["ppm"] = 999.0
        with open(path, "w") as fh:
            json.dump(data, fh)
        with self.assertRaises(SyncError) as ctx:
            load_report(path)
        self.assertEqual(ctx.exception.code, E_REPORT_MISMATCH)

    def test_version_mismatch_rejected(self):
        report = _sample_report()
        report["version"] = 999
        path = os.path.join(self.out_dir("rep4"), "report.json")
        save_report(report, path)
        with self.assertRaises(SyncError) as ctx:
            load_report(path)
        self.assertEqual(ctx.exception.code, E_REPORT_MISMATCH)

    def test_broken_chain_rejected(self):
        report = _sample_report()
        report["records"][1]["prev_hash"] = "0" * 64
        path = os.path.join(self.out_dir("rep5"), "report.json")
        save_report(report, path)
        with self.assertRaises(SyncError) as ctx:
            load_report(path)
        self.assertEqual(ctx.exception.code, E_REPORT_MISMATCH)
