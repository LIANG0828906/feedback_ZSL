import dataclasses
import unittest

from afpipe.errors import BudgetError, ShortQueryError
from afpipe.indexstore import IndexStore
from afpipe.search import Searcher

from . import CONFIG, INDEX_DIR, MANIFEST


class SearchFamilyTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.store = IndexStore(INDEX_DIR).load(
            CONFIG, manifest_fingerprint=MANIFEST.fingerprint)
        cls.searcher = Searcher(cls.store, CONFIG, MANIFEST)

    def test_lossy_query_hits_pcm_and_lossy_members(self):
        media = MANIFEST.file("q_gA_p1_mp3")
        result = self.searcher.query(media, query_id="q_gA_p1")
        self.assertTrue(result.families)
        top = result.families[0]
        self.assertEqual(top.group_id, "gA")
        kinds = {m.format_kind for m in top.members}
        self.assertEqual(kinds, {"pcm", "lossy"})
        for member in top.members:
            self.assertGreaterEqual(member.confidence,
                                    CONFIG.family_score_floor)
            self.assertLess(member.start_s, member.end_s)

    def test_pcm_query_cross_format_retrieval(self):
        media = MANIFEST.file("q_gB_p2_wav")
        result = self.searcher.query(media, query_id="q_gB_p2")
        self.assertEqual(result.families[0].group_id, "gB")
        self.assertIn("gB_long_mp3",
                      [m.file_id for m in result.families[0].members])

    def test_families_sorted_by_confidence(self):
        media = MANIFEST.file("q_gC_p1_mp3")
        result = self.searcher.query(media, query_id="q_gC_p1")
        scores = [f.confidence for f in result.families]
        self.assertEqual(scores, sorted(scores, reverse=True))

    def test_short_query_raises_E_SHORT(self):
        media = dataclasses.replace(
            MANIFEST.file("q_gA_p1_mp3"), duration_s=0.1)
        with self.assertRaises(ShortQueryError) as ctx:
            self.searcher.query(media, query_id="short")
        self.assertEqual(ctx.exception.code, "E_SHORT")

    def test_oversized_query_raises_E_BUDGET(self):
        media = dataclasses.replace(
            MANIFEST.file("q_gA_p1_mp3"), duration_s=99.0)
        with self.assertRaises(BudgetError) as ctx:
            self.searcher.query(media, query_id="long")
        self.assertEqual(ctx.exception.code, "E_BUDGET")

    def test_scan_budget_enforced(self):
        tight = CONFIG.with_overrides(max_postings_scan=10)
        searcher = Searcher(self.store, tight, MANIFEST)
        media = MANIFEST.file("q_gA_p1_mp3")
        with self.assertRaises(BudgetError) as ctx:
            searcher.query(media, query_id="tight")
        self.assertEqual(ctx.exception.code, "E_BUDGET")


if __name__ == "__main__":
    unittest.main()
