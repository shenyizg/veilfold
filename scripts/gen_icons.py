#!/usr/bin/env python3
"""Render Veilfold's application icon without third-party dependencies.

Writes:
  gui/build/appicon.png        1024x1024 PNG used by Wails for macOS and Windows
  gui/build/windows/icon.ico   multi-resolution Windows icon

The geometry matches assets/logo.svg. Shapes are drawn with signed distance
functions, which gives smooth anti-aliased edges at every size.

Usage: python3 scripts/gen_icons.py
"""

import math
import os
import struct
import zlib

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Geometry in a 1024x1024 design space (keep in sync with assets/logo.svg).
BG = (100, 100, 924, 924, 190)             # rounded square: x0, y0, x1, y1, radius
TAB = (262, 300, 492, 420, 44)             # folder tab
BODY = (262, 372, 762, 724, 60)            # folder body
KEY_CIRCLE = (512, 520, 58)                # keyhole: circle cx, cy, r
KEY_STEM = [(484, 548), (540, 548), (556, 648), (468, 648)]  # keyhole stem polygon
TOP = (0x81, 0x8C, 0xF8)                   # gradient start (top left)
BOTTOM = (0x43, 0x38, 0xCA)                # gradient end (bottom right)


def sd_round_rect(px, py, rect):
    x0, y0, x1, y1, r = rect
    cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
    bx, by = (x1 - x0) / 2 - r, (y1 - y0) / 2 - r
    qx, qy = abs(px - cx) - bx, abs(py - cy) - by
    outside = math.hypot(max(qx, 0.0), max(qy, 0.0))
    inside = min(max(qx, qy), 0.0)
    return outside + inside - r


def sd_circle(px, py, c):
    cx, cy, r = c
    return math.hypot(px - cx, py - cy) - r


def sd_convex(px, py, pts):
    # Maximum of the signed distances to each edge's half plane (points in
    # clockwise order in screen coordinates). Exact enough for anti-aliasing.
    d = -1e9
    n = len(pts)
    for i in range(n):
        x0, y0 = pts[i]
        x1, y1 = pts[(i + 1) % n]
        ex, ey = x1 - x0, y1 - y0
        length = math.hypot(ex, ey)
        nx, ny = ey / length, -ex / length
        d = max(d, (px - x0) * nx + (py - y0) * ny)
    return d


def coverage(d, scale):
    # d is in design units; convert to pixels for a one-pixel soft edge.
    return min(1.0, max(0.0, 0.5 - d * scale))


def render(size):
    scale = size / 1024.0
    rows = []
    for y in range(size):
        row = bytearray()
        py = (y + 0.5) / scale
        for x in range(size):
            px = (x + 0.5) / scale
            d_bg = sd_round_rect(px, py, BG)
            a_bg = coverage(d_bg, scale)
            if a_bg <= 0.0:
                row += b"\x00\x00\x00\x00"
                continue
            t = min(1.0, max(0.0, ((px - 100) + (py - 100)) / (2 * 824)))
            r = TOP[0] + (BOTTOM[0] - TOP[0]) * t
            g = TOP[1] + (BOTTOM[1] - TOP[1]) * t
            b = TOP[2] + (BOTTOM[2] - TOP[2]) * t

            d_folder = min(sd_round_rect(px, py, TAB), sd_round_rect(px, py, BODY))
            # Soft shadow below the folder.
            d_shadow = min(sd_round_rect(px, py - 18, TAB), sd_round_rect(px, py - 18, BODY))
            shadow = 0.22 * min(1.0, max(0.0, (24 - d_shadow) / 48))
            r, g, b = r * (1 - shadow), g * (1 - shadow), b * (1 - shadow)

            d_key = min(sd_circle(px, py, KEY_CIRCLE), sd_convex(px, py, KEY_STEM))
            a_white = coverage(max(d_folder, -d_key), scale)
            # The tab is slightly translucent to read as "behind" the body.
            if sd_round_rect(px, py, BODY) > 0 and d_folder <= 0:
                a_white *= 0.78
            r = r + (255 - r) * a_white
            g = g + (255 - g) * a_white
            b = b + (255 - b) * a_white
            row += bytes((int(r + 0.5), int(g + 0.5), int(b + 0.5), int(a_bg * 255 + 0.5)))
        rows.append(bytes(row))
    return rows


def png(rows, size):
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(kind, data):
        c = struct.pack(">I", len(data)) + kind + data
        return c + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b"")


def ico(images):
    # ICO with embedded PNGs (supported since Windows Vista).
    header = struct.pack("<HHH", 0, 1, len(images))
    offset = 6 + 16 * len(images)
    entries, blobs = b"", b""
    for size, data in images:
        dim = 0 if size >= 256 else size
        entries += struct.pack("<BBBBHHII", dim, dim, 0, 0, 1, 32, len(data), offset)
        blobs += data
        offset += len(data)
    return header + entries + blobs


def main():
    out_png = os.path.join(ROOT, "gui", "build", "appicon.png")
    out_ico = os.path.join(ROOT, "gui", "build", "windows", "icon.ico")
    os.makedirs(os.path.dirname(out_ico), exist_ok=True)
    with open(out_png, "wb") as f:
        f.write(png(render(1024), 1024))
    print("wrote", os.path.relpath(out_png, ROOT))
    images = [(s, png(render(s), s)) for s in (256, 128, 64, 48, 32, 24, 16)]
    with open(out_ico, "wb") as f:
        f.write(ico(images))
    print("wrote", os.path.relpath(out_ico, ROOT))


if __name__ == "__main__":
    main()
