from __future__ import annotations

import shutil
import tempfile
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT  # noqa: F401

from fpstore.audio import generate_single_sample
from fpstore.config import FingerprintConfig
from fpstore.library import Library, TrackSpec
from fpstore.util import TraceLogger


class DeterminismTests(unittest.TestCase):
    def _run(self, root: Path) -> bytes:
        root.mkdir(parents=True)
        samples = root / "samples"
        samples.mkdir()
        a = generate_single_sample(samples / "a.wav", seed=51, sample_rate=8000, duration_s=4.0)
        b = generate_single_sample(samples / "b.wav", seed=52, sample_rate=16000, duration_s=4.0, stereo=True)
        trace = TraceLogger(root / "trace.jsonl")
        library = Library(root / "library")
        config = FingerprintConfig(version=3, time_quant_bits=0)
        library.import_tracks([TrackSpec("a", a, config), TrackSpec("b", b, config)], trace=trace)
        library.publish(trace=trace)
        library.search(a, config=config, track_id="a", top_k=2)
        trace.emit("determinism_probe", sample="a", phase="complete")
        return (root / "trace.jsonl").read_bytes()

    def test_trace_is_byte_identical_for_same_seed_and_operations(self) -> None:
        temp = Path(tempfile.mkdtemp(prefix="fpstore-determinism-"))
        try:
            first = self._run(temp / "run-a")
            second = self._run(temp / "run-b")
            self.assertEqual(first, second)
        finally:
            shutil.rmtree(temp, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
