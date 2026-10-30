import unittest

from osp import quality


class NgramTest(unittest.TestCase):
    TRAIN = "the quick brown fox jumps over the lazy dog while the cat sleeps on the warm mat by the door"

    def test_a_copied_passage_is_found_and_a_new_one_is_not(self):
        train = quality.ngrams(self.TRAIN, 5)
        copied = "Question: The quick brown FOX jumps over the lazy dog, while the cat sleeps."
        share, compared = quality.contaminated(train, copied, 5, 0.5)
        self.assertTrue(compared)
        self.assertGreaterEqual(share, 0.5)
        fresh = "a completely different sentence about rivers mountains and the long winter nights ahead"
        share, _ = quality.contaminated(train, fresh, 5, 0.5)
        self.assertEqual(share, 0.0)

    def test_a_text_shorter_than_n_is_not_compared(self):
        share, compared = quality.contaminated(quality.ngrams(self.TRAIN, 13), "too short", 13, 0.5)
        self.assertEqual((share, compared), (0.0, False))

    def test_ngram_leakage_reads_text_members_and_records(self):
        texts = {"tr": {"a.txt": self.TRAIN, "img.png": "binary"}, "ev": {"q1.txt": self.TRAIN.upper(), "q2.txt": "nothing in common at all here " * 3}}

        class Fake:
            posted = None

            def post(self, path, body):
                Fake.posted = (path, body)
                return dict(body, state="LEAKED" if body["overlap"] else "CLEAN")

        def members(client, ds, v):
            for path in texts[v]:
                yield {"path": path, "content_type": "image/png" if path.endswith(".png") else "text/plain"}

        orig = quality.snapshots.members, quality.snapshots.fetch_member
        quality.snapshots.members = members
        quality.snapshots.fetch_member = lambda client, m: (texts["tr"] | texts["ev"])[m["path"]].encode()
        try:
            out = quality.ngram_leakage(Fake(), "d1", "tr", "d2", "ev", n=5, threshold=0.5, run_id="r1")
        finally:
            quality.snapshots.members, quality.snapshots.fetch_member = orig
        path, body = Fake.posted
        self.assertEqual(path, "/api/v1/leakage-checks")
        self.assertEqual((body["overlap"], body["eval_members"], body["run_id"]), (1, 2, "r1"))
        self.assertEqual(body["examples"][0]["eval_path"], "q1.txt")
        self.assertEqual(out["state"], "LEAKED")


class DriftLinesTest(unittest.TestCase):
    def test_breaches_are_marked(self):
        report = {"tables": [{"table": "scenes", "rows_before": 200, "rows_after": 500, "row_change": 1.5, "breached": ["rows"],
                              "columns_added": [], "columns_removed": ["band"],
                              "columns": [{"name": "reflectance", "psi": 1.234, "null_rate_before": 0, "null_rate_after": 0.2,
                                           "breached": ["distribution", "nulls"]},
                                          {"name": "id", "psi": None, "not_compared": "values are nearly all distinct, like identifiers",
                                           "null_rate_before": 0, "null_rate_after": 0, "breached": []}]}],
                  "not_compared": ["old.csv (gone)"]}
        lines = quality.drift_lines(report)
        self.assertIn("scenes: 200 -> 500 rows (+150.0%)  BREACH rows", lines[0])
        self.assertEqual(lines[1], "  removed: band")
        self.assertIn("PSI 1.234", lines[2])
        self.assertIn("BREACH distribution, nulls", lines[2])
        self.assertIn("identifiers", lines[3])
        self.assertEqual(lines[-1], "not compared: old.csv (gone)")


if __name__ == "__main__":
    unittest.main()
