import hashlib
import io
import os
import random
import tempfile
import unittest

from osp import fastcdc

MIN, AVG = fastcdc.MIN_SIZE, fastcdc.AVG_SIZE

# The same vector is in internal/platform/fastcdc and sdk/go/osp: a client that
# cuts anywhere else would find nothing to reuse.
VECTOR = "76aab42460290af8c3707a941931982e59207ffc27641b0681ceb9b96547b260"


def vector_input():
    out = bytearray()
    i = 0
    while len(out) < 6 << 20:
        out += hashlib.sha256(str(i).encode()).digest()
        i += 1
    return bytes(out[:6 << 20])


class FastCDC(unittest.TestCase):
    def test_cuts_where_the_server_does(self):
        lengths = [len(d) for _, d in fastcdc.chunks(io.BytesIO(vector_input()))]
        self.assertEqual(hashlib.sha256(",".join(map(str, lengths)).encode()).hexdigest(), VECTOR, lengths)

    @unittest.skipIf(fastcdc.numpy is None, "needs numpy")
    def test_both_chunkers_cut_alike(self):
        # Long runs of zeros reach the loose mask, and the maximum size.
        data = vector_input()[:3 << 20] + bytes(10 << 20) + vector_input()[1 << 20:3 << 20]
        rng = random.Random(5)
        noise = rng.randbytes(9 << 20)
        for sample in (data, noise, noise[:MIN + 5], noise[:AVG + 7]):
            at = 0
            while at < len(sample):
                view = memoryview(sample)[at:at + fastcdc.MAX_SIZE]
                a, b = fastcdc._cut_numpy(view), fastcdc._cut_python(view)
                self.assertEqual(a, b, at)
                at += a

    @unittest.skipIf(fastcdc.numpy is None, "needs numpy")
    def test_cutting_on_all_cores_cuts_alike(self):
        rng = random.Random(6)
        noise = rng.randbytes(30 << 20)
        # Noise, a long run with no hits at all (the maximum size), and noise again.
        for sample in (noise, noise[:14 << 20] + bytes(20 << 20) + noise[14 << 20:], noise[:(8 << 20) + 300]):
            with tempfile.TemporaryDirectory() as d:
                path = os.path.join(d, "f")
                with open(path, "wb") as f:
                    f.write(sample)
                got, size, sha = fastcdc.manifest(path)
            want = [(o, len(x), hashlib.sha256(x).hexdigest()) for o, x in fastcdc.chunks(io.BytesIO(sample))]
            self.assertEqual(size, len(sample))
            self.assertEqual(sha, hashlib.sha256(sample).hexdigest())
            self.assertEqual([(c["offset"], c["length"], c["sha256"]) for c in got], want)

    def test_chunks_rebuild_the_file(self):
        data = vector_input()[:1 << 20]
        pieces = list(fastcdc.chunks(io.BytesIO(data)))
        self.assertEqual(b"".join(d for _, d in pieces), data)
        at = 0
        for offset, d in pieces:
            self.assertEqual(offset, at)
            at += len(d)

    def test_a_small_file_is_one_chunk(self):
        self.assertEqual([d for _, d in fastcdc.chunks(io.BytesIO(b"small"))], [b"small"])
        self.assertEqual(list(fastcdc.chunks(io.BytesIO(b""))), [])


if __name__ == "__main__":
    unittest.main()
