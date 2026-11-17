import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "check-issue-references.py"
spec = importlib.util.spec_from_file_location("check_issue_references", SCRIPT)
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)

# Built at run time so that this file does not trip the check itself.
H = "#"


class IssueReferenceTests(unittest.TestCase):
    def test_a_number_after_a_hash_is_a_reference(self):
        for line in [f"see ({H}264)", f"// {H}12 fixed", f"Closes {H}1", f"refs {H}9999."]:
            self.assertTrue(check.REFERENCE.search(line), line)

    def test_colours_shell_raw_strings_and_long_numbers_are_not(self):
        for line in [f"color: {H}123abc", f"{H}fff", f"echo $${H}1", f"$" + H + "2", f"x{H}12", f"&{H}39;", f'"{H}1"',
                     f"'{H}1'", f"/{H}1", f"{{{H}1", f"{H}12345", f"{H}abc", f"{H}1a"]:
            self.assertIsNone(check.REFERENCE.search(line), line)

    def test_only_tracked_text_files_outside_links_are_read(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            subprocess.run(["git", "init", "-q", directory], check=True)
            (root / "a.go").write_text(f"package a\n\n// fixed in {H}264\n")
            (root / "b.md").write_text("nothing here\r\n" f"see {H}7\r\n")
            (root / "c.bin").write_text(f"{H}1\n")
            (root / "untracked.md").write_text(f"{H}1\n")
            (root / "link.md").symlink_to("a.go")
            subprocess.run(["git", "-C", directory, "-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "add", "a.go", "b.md", "c.bin", "link.md"], check=True)
            self.assertEqual(check.offenders(directory), ["a.go:3", "b.md:2"])
            self.assertEqual(check.main(["check", directory]), 1)
            for path in ["a.go", "b.md"]:
                (root / path).write_text("clean\n")
            self.assertEqual(check.main(["check", directory]), 0)


if __name__ == "__main__":
    unittest.main()
