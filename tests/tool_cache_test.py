"""Execute the image provisioner against local, checksum-verified fixtures."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "deploy/actions-cache/install-node-cache.sh"
VERSION = "22.22.2"


class NodeCacheTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.cache = self.root / "cache"
        self.asset = f"node-v{VERSION}-linux-x64.tar.xz"
        archive = self.root / self.asset
        with tarfile.open(archive, "w:xz") as bundle:
            body = f"#!/bin/sh\nprintf 'v{VERSION}\\n'\n".encode()
            member = tarfile.TarInfo(f"node-v{VERSION}-linux-x64/bin/node")
            member.mode = 0o755
            member.size = len(body)
            bundle.addfile(member, io.BytesIO(body))
        self.sums = self.root / "SHASUMS256.txt"
        self.sums.write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + "  " + self.asset + "\n")
        curl = self.bin / "curl"
        curl.write_text("""#!/bin/sh
set -eu
printf 'fetch\\n' >> "$FIXTURES/calls"
output=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output="$2"; shift 2 ;;
    https://*) source="${1##*/}"; shift ;;
    *) shift ;;
  esac
done
cp "$FIXTURES/$source" "$output"
""")
        curl.chmod(0o755)
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}", FIXTURES=str(self.root))

    def run_installer(self, version=VERSION):
        return subprocess.run(["sh", str(SCRIPT), version, "x64", str(self.cache)],
                              env=self.env, text=True, capture_output=True, check=False)

    def test_fresh_install_is_reused_without_network(self):
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.cache / VERSION / "x64.complete").is_file())
        self.assertTrue((self.cache / VERSION / "x64/bin/node").is_file())
        calls = (self.root / "calls").read_text()
        self.assertEqual(self.run_installer().returncode, 0)
        self.assertEqual((self.root / "calls").read_text(), calls)

    def test_bad_checksum_never_marks_cache_complete(self):
        self.sums.write_text("0" * 64 + "  " + self.asset + "\n")
        self.assertNotEqual(self.run_installer().returncode, 0)
        self.assertFalse((self.cache / VERSION / "x64.complete").exists())

    def test_invalid_version_is_rejected_before_download(self):
        self.assertNotEqual(self.run_installer("../../bad").returncode, 0)
        self.assertFalse((self.root / "calls").exists())

    def test_missing_checksum_is_rejected(self):
        self.sums.write_text("0" * 64 + "  other.tar.xz\n")
        self.assertNotEqual(self.run_installer().returncode, 0)
        self.assertFalse((self.cache / VERSION / "x64.complete").exists())

    def test_incomplete_cache_is_repaired(self):
        destination = self.cache / VERSION / "x64"
        destination.mkdir(parents=True)
        (destination / "partial").write_text("unfinished")
        result = self.run_installer()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((destination / "partial").exists())
        self.assertTrue((self.cache / VERSION / "x64.complete").exists())


if __name__ == "__main__":
    unittest.main()
