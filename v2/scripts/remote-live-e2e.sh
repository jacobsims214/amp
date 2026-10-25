#!/usr/bin/env bash
# Remote control M1 (#2379): the real Rust harness link against the real Go relay.
#
# Builds amp-tui's tests/remote_live.rs and runs TestRelay_LiveRustHarness, which serves amp-relay's router (#2387)
# on a real Postgres and runs that binary (laptop + browser) against it.
#
# Needs a Postgres (and Typesense) for the amp-api tests, as `go test ./cmd/server` does:
#   TEST_DATABASE_URL, TEST_TYPESENSE_URL, TEST_TYPESENSE_KEY
# Honours CARGO_HOME / GOCACHE / GOMODCACHE (the dev Mac keeps them on the external drive).
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
: "${TEST_DATABASE_URL:?set TEST_DATABASE_URL (a throwaway Postgres)}"
if [[ -z "${CARGO_HOME:-}" && -d /Volumes/external-data/development/.cargo-home ]]; then
  export CARGO_HOME=/Volumes/external-data/development/.cargo-home
fi

bin=$(cd "$root/v2/harness" &&
  cargo test -p amp-tui --test remote_live --locked --no-run --message-format=json 2>/dev/null |
  python3 -c 'import json,sys
for line in sys.stdin:
    m = json.loads(line)
    if m.get("reason") == "compiler-artifact" and m.get("target", {}).get("name") == "remote_live" and m.get("executable"):
        print(m["executable"])')
if [[ -z "$bin" ]]; then
  echo "could not build tests/remote_live.rs" >&2
  exit 1
fi

cd "$root/v2/amp-api"
AMP_RELAY_LIVE_HARNESS="$bin" go test -count=1 -run '^TestRelay_LiveRustHarness$' -v ./cmd/server/
