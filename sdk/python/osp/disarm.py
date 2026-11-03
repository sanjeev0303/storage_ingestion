"""Image disarm: strip what an image carries besides its picture, before upload.

The platform's image checks (docs/content-checks.md) flag or reject PNG and
JPEG files whose metadata attacks the tools that read them: a PNG text chunk
named "profile" (ImageMagick, CVE-2022-44268), private chunks, and bytes
after the image's end that make it a second file. The platform never changes
stored bytes, since their SHA-256 is what it vouches for; this cleans a file
before it is sent, keeping only what draws the picture.

    report = disarm_file("in/cat.png", "clean/cat.png")
    print(report["removed"])

PNG keeps the chunks that affect pixels (palette, transparency, colour
space, pixel size, animation); text, EXIF, time and private chunks go. JPEG
keeps its JFIF header, ICC profile, Adobe colour marker and the coded image;
EXIF and XMP (APP1, including orientation), other application segments and
comments go. Anything after the image's end goes, including the extra
pictures of a multi-picture (MPF) file.
"""

import struct
import zlib

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
PNG_KEEP = {b"IHDR", b"PLTE", b"IDAT", b"IEND", b"tRNS", b"gAMA", b"cHRM", b"sRGB", b"iCCP", b"cICP",
            b"sBIT", b"pHYs", b"mDCV", b"cLLI", b"acTL", b"fcTL", b"fdAT"}


def disarm_png(data):
    """(clean bytes, [removed chunk types]) of a PNG; ValueError if it is broken."""
    if not data.startswith(PNG_MAGIC):
        raise ValueError("not a PNG")
    out, removed, pos = [PNG_MAGIC], [], len(PNG_MAGIC)
    while True:
        if pos + 8 > len(data):
            raise ValueError("the PNG ends before its IEND chunk")
        length, kind = struct.unpack(">I4s", data[pos:pos + 8])
        end = pos + 12 + length
        if end > len(data):
            raise ValueError(f"chunk {kind!r} is cut short")
        crc = struct.unpack(">I", data[end - 4:end])[0]
        if zlib.crc32(data[pos + 4:end - 4]) != crc:
            raise ValueError(f"chunk {kind!r} fails its CRC check")
        if kind in PNG_KEEP:
            out.append(data[pos:end])
        else:
            removed.append(kind.decode("latin-1"))
        pos = end
        if kind == b"IEND":
            break
    if pos < len(data):
        removed.append(f"{len(data) - pos} bytes after IEND")
    return b"".join(out), removed


# APP0 (JFIF), APP2 only as an ICC profile, APP14 (Adobe colour transform).
def _keep_jpeg_segment(marker, body):
    if 0xE0 <= marker <= 0xEF:
        return (marker == 0xE0 and body.startswith(b"JFIF\x00")) or \
               (marker == 0xE2 and body.startswith(b"ICC_PROFILE\x00")) or \
               (marker == 0xEE and body.startswith(b"Adobe"))
    return marker != 0xFE  # comments go


def disarm_jpeg(data):
    """(clean bytes, [removed segments]) of a JPEG; ValueError if it is broken."""
    if not data.startswith(b"\xff\xd8"):
        raise ValueError("not a JPEG")
    out, removed, pos = [b"\xff\xd8"], [], 2
    while True:
        if pos >= len(data) or data[pos] != 0xFF:
            raise ValueError("the JPEG ends or breaks before its end-of-image marker")
        while pos < len(data) and data[pos] == 0xFF:
            pos += 1
        if pos >= len(data):
            raise ValueError("the JPEG ends inside a marker")
        marker = data[pos]
        pos += 1
        if marker == 0xD9:
            out.append(b"\xff\xd9")
            break
        if marker == 0x01 or 0xD0 <= marker <= 0xD7:
            out.append(bytes([0xFF, marker]))
            continue
        if pos + 2 > len(data):
            raise ValueError("the JPEG ends inside a segment")
        length = struct.unpack(">H", data[pos:pos + 2])[0]
        if length < 2 or pos + length > len(data):
            raise ValueError(f"segment FF{marker:02X} has an impossible length")
        body = data[pos + 2:pos + length]
        segment = bytes([0xFF, marker]) + data[pos:pos + length]
        pos += length
        if not _keep_jpeg_segment(marker, body):
            removed.append(f"FF{marker:02X}")
            continue
        out.append(segment)
        if marker == 0xDA:  # entropy-coded data up to the next real marker
            start = pos
            while True:
                i = data.find(b"\xff", pos)
                if i < 0 or i + 1 >= len(data):
                    raise ValueError("the JPEG ends inside its image data")
                nxt = data[i + 1]
                if nxt == 0x00 or 0xD0 <= nxt <= 0xD7 or nxt == 0xFF:
                    pos = i + 1 if nxt == 0xFF else i + 2
                    continue
                pos = i
                break
            out.append(data[start:pos])
    if pos < len(data):
        removed.append(f"{len(data) - pos} bytes after the end-of-image marker")
    return b"".join(out), removed


def disarm_bytes(data):
    """(kind, clean bytes, removed) for a PNG or JPEG; ValueError otherwise."""
    if data.startswith(PNG_MAGIC):
        return ("png",) + disarm_png(data)
    if data.startswith(b"\xff\xd8\xff"):
        return ("jpeg",) + disarm_jpeg(data)
    raise ValueError("not a PNG or JPEG")


def disarm_file(src, dst):
    """Writes src's disarmed image to dst and says what was removed."""
    with open(src, "rb") as f:
        data = f.read()
    kind, clean, removed = disarm_bytes(data)
    with open(dst, "wb") as f:
        f.write(clean)
    return {"kind": kind, "before": len(data), "after": len(clean), "removed": removed}
