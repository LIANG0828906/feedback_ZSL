import unittest

import numpy as np

from opera_master import PART_BANGU, PART_BANGQIANG, PART_BED
from opera_master.decouple import run_decoupling
from opera_master.synthesis import apply_crosstalk, build_clean_sources

from .helpers import make_workspace


class DecouplingTests(unittest.TestCase):
    def setUp(self):
        self.tmp, self.cue_path, self.cue = make_workspace(shared=True)
        self.clean = build_clean_sources(self.cue)
        self.injected = {k: v.copy() for k, v in self.clean.items()}
        apply_crosstalk(self.injected, self.cue)

    def test_declared_crosstalk_is_detected_and_suppressed(self):
        out, reports = run_decoupling(self.injected, self.cue)
        by_channel = {r["channel"]: r for r in reports}
        for victim in (PART_BANGQIANG, PART_BANGU):
            report = by_channel[victim]
            self.assertTrue(report["detected"], victim)
            self.assertTrue(report["modified"], victim)
            # The crosstalk component itself must be heavily attenuated.
            self.assertGreaterEqual(report["crosstalk_attenuation_db"], 10.0)

    def test_vocal_tail_loss_within_one_db(self):
        out, _ = run_decoupling(self.injected, self.cue)
        from opera_master.decouple import tail_energy_loss_db

        loss = tail_energy_loss_db(
            self.injected[PART_BANGQIANG],
            out[PART_BANGQIANG],
            self.cue.sample_rate,
            self.cue.reference_times[0] + 0.6,
        )
        self.assertIsNotNone(loss)
        self.assertLessEqual(abs(loss), 1.0)

    def test_percussion_peak_loss_within_one_db(self):
        out, _ = run_decoupling(self.injected, self.cue)
        from opera_master.decouple import peak_loss_db

        for part in (PART_BANGU,):
            loss = peak_loss_db(
                self.injected[part],
                out[part],
                self.cue.sample_rate,
                self.cue.reference_times,
            )
            self.assertGreaterEqual(loss, -1.0)
            self.assertLessEqual(loss, 1.0)

    def test_signal_outside_window_untouched(self):
        out, _ = run_decoupling(self.injected, self.cue)
        sr = self.cue.sample_rate
        for ch in self.cue.channels:
            if not ch.crosstalk_source:
                continue
            we = int(round(ch.crosstalk_window[1] * sr))
            n = self.cue.total_samples
            if we < n:
                np.testing.assert_array_equal(
                    out[ch.part][we + 10 :],
                    self.injected[ch.part][we + 10 :],
                )

    def test_uninjected_channels_unchanged(self):
        out, _ = run_decoupling(self.injected, self.cue)
        for part in (PART_BED,):
            np.testing.assert_array_equal(out[part], self.injected[part])

    def test_gong_channel_not_modified_by_decoupling(self):
        out, reports = run_decoupling(self.injected, self.cue)
        modified_victims = {r["channel"] for r in reports if r["modified"]}
        self.assertNotIn("大锣", modified_victims)


if __name__ == "__main__":
    unittest.main()
