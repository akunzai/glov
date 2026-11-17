from pathlib import Path
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]


class VersionTests(unittest.TestCase):
    def test_version_file_is_semver(self):
        self.assertRegex((ROOT / "VERSION").read_text().strip(), r"^\d+\.\d+\.\d+$")

    def test_build_label_starts_with_the_version_file(self):
        base = (ROOT / "VERSION").read_text().strip()
        label = subprocess.run([str(ROOT / "tools/version.sh")], capture_output=True, text=True, check=True).stdout.strip()
        self.assertRegex(label, "^" + re.escape(base) + r"(\+[0-9a-f]+(-dirty)?)?$")


if __name__ == "__main__":
    unittest.main()
