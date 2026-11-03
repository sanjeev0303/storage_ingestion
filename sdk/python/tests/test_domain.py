import unittest

from osp import domain


class FakeClient:
    def __init__(self):
        self.posts = []
        self.pages = []

    def post(self, path, body):
        self.posts.append((path, body))
        return {"written": len(body.get("vectors", []))}

    def request(self, method, path, params=None):
        return {"segments": self.pages.pop(0)}


class Domain(unittest.TestCase):
    def test_vectors_go_in_batches(self):
        c = FakeClient()
        n = domain.put_vectors(c, "x", ({"path": f"{i}.jpg", "vector": [i, 1]} for i in range(5)), batch=2)
        self.assertEqual(n, 5)
        self.assertEqual([len(b["vectors"]) for _, b in c.posts], [2, 2, 1])

    def test_segments_follow_pages(self):
        c = FakeClient()
        c.pages = [[{"id": str(i)} for i in range(1000)], [{"id": "last"}]]
        got = list(domain.segments(c, "d", "v"))
        self.assertEqual(len(got), 1001)
        self.assertEqual(got[-1]["id"], "last")

    def test_creator_text(self):
        self.assertEqual(domain.parse_creator("Ada Lovelace"), {"name": "Ada Lovelace"})
        self.assertEqual(domain.parse_creator("Ada Lovelace (Analytical Engines Ltd)"), {"name": "Ada Lovelace", "affiliation": "Analytical Engines Ltd"})
        self.assertEqual(domain.parse_creator("Ada (Lab) 0000-0002-1825-009X"), {"name": "Ada", "affiliation": "Lab", "orcid": "0000-0002-1825-009X"})


if __name__ == "__main__":
    unittest.main()
