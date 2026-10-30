import base64
import io
import json
import unittest

from osp import evidence


def chain(n, tamper=None):
    """n exported entries, chained as the server chains them."""
    lines, prev = [], b""
    for seq in range(1, n + 1):
        e = {"seq": seq, "id": f"e{seq}", "chain": "org1", "tenant_id": "org1", "actor_type": "USER", "actor_id": "u1",
             "action": "asset.delete" if seq % 2 else "retention.policy_set", "resource_type": "asset", "resource_id": f"a{seq}",
             "outcome": "SUCCESS", "reason": "", "correlation_id": "c", "source_ip": "10.0.0.1",
             "created_at": f"2026-10-30T10:00:0{seq % 10}.5Z", "details_json": json.dumps({"n": seq}, separators=(",", ":")),
             "prev_hash": prev.hex()}
        e["details"] = json.loads(e["details_json"])
        e["hash"] = evidence.entry_hash(e, prev)
        prev = bytes.fromhex(e["hash"])
        lines.append(e)
    if tamper:
        tamper(lines)
    return [json.dumps(e) for e in lines]


class VerifyTest(unittest.TestCase):
    def test_an_intact_export_verifies(self):
        r = evidence.verify_export(io.StringIO("\n".join(chain(6))))
        self.assertEqual((r["checked"], r["first"], r["last"], r["broken_at"]), (6, 1, 6, None))

    def test_a_changed_entry_is_found(self):
        def change(lines):
            lines[3]["actor_id"] = "someone-else"
        r = evidence.verify_export(chain(6, change))
        self.assertEqual((r["broken_at"], r["problem"]), (4, "the entry does not match its hash"))

    def test_a_removed_entry_is_found(self):
        r = evidence.verify_export(chain(6, lambda lines: lines.pop(2)))
        self.assertEqual(r["broken_at"], 4)
        self.assertIn("missing", r["problem"])

    def test_a_regenerated_tail_is_caught_by_its_anchor(self):
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
        key = Ed25519PrivateKey.generate()
        public = base64.b64encode(key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)).decode()
        original = [json.loads(l) for l in chain(5)]
        body = json.dumps({"seq": 5, "hash": original[4]["hash"]})
        anchor = {"body": body, "signature": base64.b64encode(key.sign(body.encode())).decode()}
        r = evidence.verify_export(chain(5), [anchor], public)
        self.assertTrue(r["anchors"][0]["ok"])

        # Someone with database access rewrites entry 3 and re-hashes the
        # rest: the chain verifies on its own, the anchor does not.
        def rewrite(lines):
            lines[2]["actor_id"] = "admin"
            prev = bytes.fromhex(lines[2]["prev_hash"])
            for e in lines[2:]:
                e["prev_hash"] = prev.hex()
                e["hash"] = evidence.entry_hash(e, prev)
                prev = bytes.fromhex(e["hash"])
        forged = evidence.verify_export(chain(5, rewrite), [anchor], public)
        self.assertIsNone(forged["broken_at"])
        self.assertEqual(forged["anchors"][0], {"seq": 5, "ok": False, "why": "the chain differs from what was anchored"})
        bad_sig = dict(anchor, signature=base64.b64encode(b"x" * 64).decode())
        self.assertFalse(evidence.verify_export(chain(5), [bad_sig], public)["anchors"][0]["ok"])


if __name__ == "__main__":
    unittest.main()
