from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

from tests.helpers import PROJECT_ROOT  # noqa: F401

from fpstore.audio import generate_single_sample
from fpstore.config import FingerprintConfig
from fpstore.errors import E_ALIGN_MISMATCH
from fpstore.library import Library, TrackSpec
from fpstore.util import canonical_bytes, content_hash, hash_label, make_chain


class AlignmentSearchTests(unittest.TestCase):
    def test_incompatible_candidate_is_marked_and_excluded_from_votes(self) -> None:
        temp = Path(tempfile.mkdtemp(prefix="fpstore-align-"))
        try:
            wav = generate_single_sample(temp / "a.wav", seed=61, sample_rate=8000, duration_s=4.0)
            library = Library(temp / "library")
            config = FingerprintConfig(version=1, time_quant_bits=None)
            library.import_tracks([TrackSpec("a", wav, config)])
            library.publish()

            pointer = json.loads(library.published_pointer.read_text())
            manifest_path = library.root / pointer["manifest_file"]
            manifest = json.loads(manifest_path.read_text())
            for entry in manifest["entries"]:
                entry["time_basis"]["sample_rate"] = 16000
            chained, root = make_chain(
                ({key: value for key, value in entry.items() if key not in {"chain_hash"}} for entry in manifest["entries"]),
                manifest["parent_manifest_hash"] or "GENESIS",
            )
            manifest["entries"] = chained
            manifest["root_chain_hash"] = root
            core = {key: value for key, value in manifest.items() if key != "manifest_hash"}
            new_hash = hash_label(content_hash(core))
            manifest["manifest_hash"] = new_hash
            new_path = library._manifest_path(new_hash)
            new_path.write_bytes(canonical_bytes(manifest))
            pointer["manifest_hash"] = new_hash
            pointer["manifest_file"] = f"manifests/{new_path.name}"
            library.published_pointer.write_bytes(canonical_bytes(pointer))

            result = library.search(wav, config=config, top_k=2)
            self.assertEqual(result["candidates"], [])
            self.assertTrue(result["alignment_errors"])
            self.assertEqual(result["alignment_errors"][0]["code"], E_ALIGN_MISMATCH)
            self.assertTrue(result["alignment_errors"][0]["excluded"])
            self.assertEqual(result["alignment_errors"][0]["sample"], "a")
            self.assertTrue(result["snapshot_released"])
        finally:
            shutil.rmtree(temp, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
