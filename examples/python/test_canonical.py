#!/usr/bin/env python3
"""The canonical form against the vectors the Go implementation also passes.

    python3 -m unittest test_canonical     (from examples/python; no paho needed)
"""

import base64
import json
import pathlib
import unittest

from canonical import CanonicalError, canonical

VECTORS = pathlib.Path(__file__).resolve().parents[2] / "a2a" / "testdata" / "canonical-vectors.json"


class Vectors(unittest.TestCase):
    def test_vectors(self):
        doc = json.loads(VECTORS.read_text(encoding="utf-8"))
        self.assertGreaterEqual(len(doc["vectors"]), 20)
        for v in doc["vectors"]:
            wire = base64.b64decode(v["wire_base64"]) if "wire_base64" in v else v["wire"].encode("utf-8")
            with self.subTest(v["name"]):
                if v.get("error"):
                    with self.assertRaises(CanonicalError):
                        canonical(wire)
                    continue
                got = canonical(wire)
                self.assertEqual(got.decode("utf-8"), v["canonical"])
                # Canonical output canonicalizes to itself.
                self.assertEqual(canonical(got), got)


if __name__ == "__main__":
    unittest.main()
