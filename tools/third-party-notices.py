#!/usr/bin/env python3
"""Write the licence and notice texts of the third-party code a release archive's binaries link.

Release archives ship compiled dependencies, whose licences (Apache-2.0's section 4 among them) ask for their licence
and NOTICE texts to travel with the binaries. Run after the build, so SwiftPM's checkouts and Go's module cache hold
the sources.

Usage: third-party-notices.py -o <file> [--swift] [--go <module dir>:<GOOS> ...]  (from the repository root)
  --swift              every package pinned in Package.resolved, from .build/checkouts
  --go guest:linux     the modules `go list -deps ./...` reaches in guest/ for GOOS=linux
"""

import argparse
import json
import os
import re
import subprocess
import sys

LICENSE_FILE = re.compile(r"^(licen[cs]e|copying|notice|patents)([.-].*)?$", re.IGNORECASE)
OWN_MODULE = "github.com/akunzai/glov"


def licence_files(directory):
    names = sorted(n for n in os.listdir(directory) if LICENSE_FILE.match(n) and os.path.isfile(os.path.join(directory, n)))
    if not names:
        sys.exit(f"no licence file in {directory}")
    return [(n, open(os.path.join(directory, n), encoding="utf-8", errors="replace").read()) for n in names]


def swift_packages():
    with open("Package.resolved", encoding="utf-8") as f:
        pins = json.load(f)["pins"]
    for pin in pins:
        location = pin["location"]
        checkout = re.sub(r"\.git$", "", location.rstrip("/").rsplit("/", 1)[-1])
        version = pin["state"].get("version") or pin["state"]["revision"][:12]
        yield location, version, os.path.join(".build", "checkouts", checkout)


def go_modules(spec):
    directory, _, goos = spec.partition(":")
    env = dict(os.environ, GOOS=goos) if goos else None
    template = "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}"
    listing = subprocess.run(["go", "list", "-deps", "-f", template, "./..."], cwd=directory, env=env,
                             check=True, capture_output=True, text=True).stdout
    for line in listing.splitlines():
        if line and not line.startswith(OWN_MODULE):
            path, version, module_dir = line.split("\t")
            yield path, version, module_dir


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("-o", "--output", required=True)
    parser.add_argument("--swift", action="store_true")
    parser.add_argument("--go", action="append", default=[])
    args = parser.parse_args()

    found = {}
    if args.swift:
        for name, version, directory in swift_packages():
            found[name] = (version, directory)
    for spec in args.go:
        for name, version, directory in go_modules(spec):
            found[name] = (version, directory)

    out = ["# Third-party notices", "",
           "glov is licensed under the Apache License 2.0 (LICENSE). Its binaries include the following code, under",
           "the licences below.", ""]
    for name in sorted(found, key=str.lower):
        version, directory = found[name]
        out += [f"## {name} {version}", ""]
        for file_name, text in licence_files(directory):
            out += [f"### {file_name}", "", "```text", text.rstrip("\n"), "```", ""]
    with open(args.output, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(out))


if __name__ == "__main__":
    main()
