import dataclasses
import unittest

from afpipe.errors import ChannelError, DecodeError, MetaError
from afpipe.manifest import dump_manifest
from afpipe.pipeline import process_file

from . import CONFIG, CORPUS_DIR, MANIFEST, MANIFEST_PATH, fresh_dir
import json


class DecodeNormalisationTest(unittest.TestCase):
    def test_pcm_and_lossy_decode_to_float(self):
        for media in MANIFEST.index_files()[:2]:
            processed = process_file(media, MANIFEST, CONFIG)
            self.assertEqual(processed.audio.sample_rate, CONFIG.sample_rate)
            self.assertEqual(processed.audio.channels, 1)
            self.assertEqual(processed.audio.samples.dtype.name, "float32")
            self.assertLessEqual(abs(processed.audio.samples).max(), 1.0)

    def test_duration_within_tolerance(self):
        media = MANIFEST.file("gA_long_mp3")
        processed = process_file(media, MANIFEST, CONFIG)
        self.assertLessEqual(
            abs(processed.audio.duration_s - media.duration_s),
            CONFIG.duration_tolerance_s + 1e-6,
        )

    def test_wrong_declared_sr_returns_E_META_with_field(self):
        with open(MANIFEST_PATH) as fh:
            raw = json.load(fh)
        raw["groups"]["gA"]["files"][0]["sample_rate"] = 8000
        path = fresh_dir("meta") + ".json"
        dump_manifest(raw, path)
        from afpipe.manifest import load_manifest

        broken = load_manifest(path)
        with self.assertRaises(MetaError) as ctx:
            process_file(broken.file("gA_long_wav"), broken, CONFIG)
        self.assertEqual(ctx.exception.code, "E_META")
        self.assertEqual(ctx.exception.details["problems"][0]["field"],
                         "sample_rate")

    def test_missing_file_returns_E_DECODE(self):
        media = dataclasses.replace(
            MANIFEST.file("gB_long_wav"),
            path="does/not/exist.wav",
        )
        with self.assertRaises(DecodeError) as ctx:
            process_file(media, MANIFEST, CONFIG)
        self.assertEqual(ctx.exception.code, "E_DECODE")

    def test_channel_cap_returns_E_CHAN(self):
        media = dataclasses.replace(MANIFEST.file("gC_long_wav"), channels=6)
        import afpipe.ffmpeg as ffmod
        import afpipe.pipeline as pmod

        original = ffmod.probe

        def fake_probe(path):
            info = original(path)
            if path.endswith("gC_long.wav"):
                info["channels"] = 6
            return info

        ffmod.probe = fake_probe
        pmod.probe = fake_probe
        try:
            with self.assertRaises(ChannelError) as ctx:
                process_file(media, MANIFEST,
                             CONFIG.with_overrides(max_channels=2))
            self.assertEqual(ctx.exception.code, "E_CHAN")
        finally:
            ffmod.probe = original
            pmod.probe = original


if __name__ == "__main__":
    unittest.main()
