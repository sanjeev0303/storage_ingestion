import hashlib
import unittest

from osp import chunks


def chunk(offset, data):
    return {"offset": offset, "length": len(data), "sha256": hashlib.sha256(data).hexdigest()}


class Plan(unittest.TestCase):
    def test_missing_chunks_merge_into_one_range(self):
        a, b, c, d = b"aaaa", b"bb", b"ccc", b"dd"
        wanted = [chunk(0, a), chunk(4, b), chunk(6, c), chunk(9, d)]
        local = {wanted[0]["sha256"]: ("old", 100), wanted[3]["sha256"]: ("old", 200)}
        steps = chunks.plan(wanted, local)
        self.assertEqual(len(steps), 3)
        self.assertEqual(steps[0]["from"], ("old", 100))
        self.assertEqual((steps[1]["offset"], steps[1]["length"]), (4, 5))
        self.assertEqual(steps[2]["from"], ("old", 200))


if __name__ == "__main__":
    unittest.main()
