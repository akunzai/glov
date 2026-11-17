#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

set --
clt_testing=false
developer_dir="$(xcode-select -p)"
frameworks="$developer_dir/Library/Developer/Frameworks"
plugins="$developer_dir/usr/lib/swift/host/plugins/testing"

if [[ "$developer_dir" == */CommandLineTools && -d "$frameworks/Testing.framework" && -f "$plugins/libTestingMacros.dylib" ]]; then
    # CLT's Testing layout needs explicit search paths; swiftbuild also emits unsigned executables here.
    # Native remains the compatibility backend: https://forums.swift.org/t/85548
    set -- --build-system native \
        -Xswiftc -F -Xswiftc "$frameworks" \
        -Xswiftc -plugin-path -Xswiftc "$plugins" \
        -Xlinker -rpath -Xlinker "$frameworks"
    clt_testing=true
    echo 'verify: using native backend for the Command Line Tools Testing layout'
fi

# A test whose thread is blocked outlives its own .timeLimit, so each test run gets a wall-clock limit. On expiry the
# test processes are sampled into .build/verify/ and the tests that started but never finished are named.
test_limit="${VERIFY_TEST_TIMEOUT:-900}"

descendants() {
    local child
    for child in $(pgrep -P "$1"); do
        echo "$child"
        descendants "$child"
    done
}

run_tests() {
    local log pid elapsed=0 p
    mkdir -p .build/verify
    log=.build/verify/test.log
    "$@" > >(tee "$log") 2>&1 &
    pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        if (( elapsed >= test_limit )); then
            printf '\nverify: timeout: %s ran over %ss (VERIFY_TEST_TIMEOUT)\n' "$*" "$test_limit"
            for p in $(descendants "$pid"); do
                sample "$p" 3 -file ".build/verify/sample-$p.txt" >/dev/null 2>&1 && echo "verify: sampled $p into .build/verify/sample-$p.txt"
            done
            echo 'verify: tests started but not finished:'
            # Swift Testing: "Test name() started." / "passed"; XCTest: "Test Case '-[Suite test]' started." / "passed (".
            awk 'match($0, /Test (Case .*\]'"'"'|[^ ]+\(.*\)) (started|passed|failed|skipped)/) {
                    text = substr($0, RSTART, RLENGTH); state = text; sub(/.* /, "", state); name = substr(text, 1, length(text) - length(state) - 1)
                    if (state == "started") open[name] = 1; else delete open[name]
                } END { for (name in open) print "  " name }' "$log"
            kill $(descendants "$pid") "$pid" 2>/dev/null || true
            exit 1
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done
    wait "$pid"
}

# CI's references job, here first: it fails fast and runs on every change, Swift or not.
python3 -B tools/check-issue-references.py
swift build "$@"
if "$clt_testing"; then set -- "$@" --disable-xctest; fi
run_tests swift test "$@"
run_tests env LIBDISPATCH_COOPERATIVE_POOL_STRICT=1 swift test "$@" --skip-build
echo 'verify: ok'
