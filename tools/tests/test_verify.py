import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


VERIFY = Path(__file__).resolve().parents[1] / "verify.sh"


class VerificationTests(unittest.TestCase):
    def run_verification(self, clt=False, fail_at=""):
        with tempfile.TemporaryDirectory(prefix="glov verify ") as directory:
            root = Path(directory)
            developer = root / ("CommandLineTools" if clt else "Xcode.app/Contents/Developer")
            if clt:
                (developer / "Library/Developer/Frameworks/Testing.framework").mkdir(parents=True)
                plugins = developer / "usr/lib/swift/host/plugins/testing"
                plugins.mkdir(parents=True)
                (plugins / "libTestingMacros.dylib").touch()
            stub = f"#!{sys.executable}\n" + '''import json, os, pathlib, sys
if pathlib.Path(sys.argv[0]).name == "xcode-select":
    print(os.environ["TEST_DEVELOPER_DIR"])
else:
    with open(os.environ["TEST_LOG"], "a") as log:
        log.write(json.dumps([sys.argv[1:], os.getenv("LIBDISPATCH_COOPERATIVE_POOL_STRICT")]) + "\\n")
    if sys.argv[1] == os.environ["TEST_FAIL_AT"]:
        sys.exit(23)
'''
            for name in ("swift", "xcode-select"):
                path = root / name
                path.write_text(stub)
                path.chmod(0o755)
            log = root / "calls.jsonl"
            env = dict(os.environ, PATH=f"{root}:{os.environ['PATH']}",
                       TEST_DEVELOPER_DIR=str(developer), TEST_LOG=str(log), TEST_FAIL_AT=fail_at)
            env.pop("LIBDISPATCH_COOPERATIVE_POOL_STRICT", None)
            result = subprocess.run(["/bin/bash", str(VERIFY)], cwd=root, env=env,
                                    capture_output=True, text=True)
            calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            return result, calls, str(developer)

    def test_default_toolchain_runs_both_test_modes(self):
        result, calls, _ = self.run_verification()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [[["build"], None], [["test"], None],
                                 [["test", "--skip-build"], "1"]])
        self.assertIn("verify: ok", result.stdout)

    def test_clt_layout_supplies_testing_paths_including_spaces(self):
        result, calls, developer = self.run_verification(clt=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 3)
        for arguments, _ in calls:
            self.assertIn("native", arguments)
            self.assertIn(developer + "/Library/Developer/Frameworks", arguments)
            self.assertIn(developer + "/usr/lib/swift/host/plugins/testing", arguments)
        self.assertNotIn("--disable-xctest", calls[0][0])
        self.assertTrue(all("--disable-xctest" in call[0] for call in calls[1:]))
        self.assertEqual(calls[-1][1], "1")

    def test_failure_stops_without_retrying_another_backend(self):
        for clt in (False, True):
            for command, count in (("build", 1), ("test", 2)):
                with self.subTest(clt=clt, command=command):
                    result, calls, _ = self.run_verification(clt=clt, fail_at=command)
                    self.assertEqual(result.returncode, 23)
                    self.assertEqual(len(calls), count)
                    self.assertNotIn("verify: ok", result.stdout)


if __name__ == "__main__":
    unittest.main()
