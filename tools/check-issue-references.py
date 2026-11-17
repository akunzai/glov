#!/usr/bin/env python3
"""Fail when a tracked file points at an issue or pull request by number.

Numbers from the repository this code was moved from do not match this one's,
and a file should stand on its own, so a `#` followed by digits (as in a
parenthesized reference) must describe the problem instead. Commit messages and PR bodies are not
tracked files and may link issues.

Usage: check-issue-references.py [repo root]  (default: the current directory)
"""

import os
import re
import subprocess
import sys

TEXT_EXTENSIONS = {"swift", "go", "md", "sh", "ps1", "py", "json", "yml", "yaml", "toml", "txt", "cs", "xml", "mod"}

# `#` then 1-4 digits, not part of a word, a hex colour, a shell `$#` or a Swift raw string.
REFERENCE = re.compile(r"""(?<![\w&$'"/{])#\d{1,4}\b(?![0-9a-fA-F])""")


def offenders(root):
    listing = subprocess.run(["git", "-C", root, "ls-files", "-z"], check=True, capture_output=True).stdout
    found = []
    for path in listing.decode("utf-8").split("\0"):
        if not path or os.path.splitext(path)[1].lstrip(".") not in TEXT_EXTENSIONS:
            continue
        full = os.path.join(root, path)
        # A symlink such as Sources/glov/Guide/*.md is checked through its target.
        if os.path.islink(full):
            continue
        try:
            with open(full, encoding="utf-8", newline="") as f:
                text = f.read()
        except (OSError, UnicodeDecodeError):
            continue
        for number, line in enumerate(text.split("\n"), 1):
            if REFERENCE.search(line):
                found.append(f"{path}:{number}")
    return found


def main(argv):
    found = offenders(argv[1] if len(argv) > 1 else ".")
    for place in found:
        print(f"{place}: describe the problem instead of citing an issue or pull request number")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
