import struct
import unittest
import zlib

from osp.disarm import PNG_MAGIC, disarm_bytes, disarm_jpeg, disarm_png


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


def png(*extra, trailing=b""):
    ihdr = chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 0, 0, 0, 0))
    idat = chunk(b"IDAT", zlib.compress(b"\x00\x7f"))
    return PNG_MAGIC + ihdr + b"".join(extra) + idat + chunk(b"IEND", b"") + trailing


def segment(marker, body):
    return bytes([0xFF, marker]) + struct.pack(">H", len(body) + 2) + body


def jpeg(trailing=b""):
    return (b"\xff\xd8" + segment(0xE0, b"JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
            + segment(0xE1, b"Exif\x00\x00<?php ?>") + segment(0xFE, b"a comment")
            + segment(0xDB, b"\x00" + bytes(64)) + segment(0xDA, b"\x01\x01\x00\x00\x3f\x00")
            + b"\x12\xff\x00\x34\xff\xd0\x56" + b"\xff\xd9" + trailing)


class DisarmTest(unittest.TestCase):
    def test_png_keeps_the_picture_and_drops_the_rest(self):
        dirty = png(chunk(b"tEXt", b"profile\x00/etc/passwd"), chunk(b"prVt", b"x"), chunk(b"gAMA", b"\x00\x00\xb1\x8f"),
                    trailing=b"PK\x03\x04zip")
        clean, removed = disarm_png(dirty)
        self.assertEqual(removed, ["tEXt", "prVt", "7 bytes after IEND"])
        self.assertEqual(clean, png(chunk(b"gAMA", b"\x00\x00\xb1\x8f")))
        self.assertEqual(disarm_png(clean)[1], [], "a clean file stays as it is")

    def test_png_that_is_broken_is_refused(self):
        bad = bytearray(png())
        bad[20] ^= 1
        with self.assertRaises(ValueError):
            disarm_png(bytes(bad))
        with self.assertRaises(ValueError):
            disarm_png(png()[:-5])

    def test_jpeg_drops_exif_comments_and_riders(self):
        clean, removed = disarm_jpeg(jpeg(trailing=b"<script>"))
        self.assertEqual(removed, ["FFE1", "FFFE", "8 bytes after the end-of-image marker"])
        self.assertNotIn(b"Exif", clean)
        self.assertNotIn(b"comment", clean)
        self.assertTrue(clean.endswith(b"\x12\xff\x00\x34\xff\xd0\x56\xff\xd9"), "scan data with stuffing and restarts is kept")
        self.assertEqual(disarm_jpeg(clean)[1], [])

    def test_other_files_are_refused(self):
        with self.assertRaises(ValueError):
            disarm_bytes(b"GIF89a")


if __name__ == "__main__":
    unittest.main()
