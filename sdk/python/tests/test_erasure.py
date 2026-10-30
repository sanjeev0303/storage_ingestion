import base64
import hashlib
import io
import json
import unittest

from osp import erasure


class CertificateTest(unittest.TestCase):
    def setUp(self):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        self.key = Ed25519PrivateKey.generate()
        self.public = base64.b64encode(self.key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)).decode()

    def cert(self, body):
        text = json.dumps(body)
        return {"body": text, "signature": base64.b64encode(self.key.sign(text.encode())).decode(), "key_id": "k1",
                "public_key": self.public, "body_sha256": hashlib.sha256(text.encode()).hexdigest()}

    def test_a_signed_certificate_verifies(self):
        body = erasure.verify_certificate(self.cert({"erasure_id": "e1", "destroyed": [{"sha256": "ab"}]}))
        self.assertEqual(body["erasure_id"], "e1")

    def test_an_edited_certificate_does_not(self):
        c = self.cert({"erasure_id": "e1", "destroyed": []})
        c["body"] = c["body"].replace('"destroyed": []', '"destroyed": [{"sha256": "x"}]')
        with self.assertRaises(ValueError):
            erasure.verify_certificate(c)

    def test_a_certificate_from_another_key_does_not(self):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        other = Ed25519PrivateKey.generate().public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
        with self.assertRaises(ValueError):
            erasure.verify_certificate(self.cert({"erasure_id": "e1"}), base64.b64encode(other).decode())


class SelectorTest(unittest.TestCase):
    def test_hashes_come_from_a_manifest(self):
        manifest = io.StringIO('{"path": "a.txt", "sha256": "AB12"}\n\n{"path": "b.txt", "sha256": "cd34"}\n')
        self.assertEqual(erasure.sha256s_from_manifest(manifest), ["ab12", "cd34"])

    def test_create_sends_the_selection(self):
        class Fake:
            def post(self, path, body):
                self.sent = (path, body)
                return {"id": "e1"}
        f = Fake()
        erasure.create(f, "COPYRIGHT", "settlement", sha256s=["AB"], institution="Court", reference="3:24-cv-05417")
        path, body = f.sent
        self.assertEqual(path, "/api/v1/erasures")
        self.assertEqual(body, {"subject_ids": [], "sha256s": ["ab"], "object_ids": [], "kind": "COPYRIGHT",
                                "reason": "settlement", "reference": "3:24-cv-05417", "institution": "Court"})

    def test_subject_hash(self):
        self.assertEqual(erasure.subject_hash("P-1"), hashlib.sha256(b"P-1").hexdigest())


if __name__ == "__main__":
    unittest.main()
