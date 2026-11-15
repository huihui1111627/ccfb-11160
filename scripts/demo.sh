#!/usr/bin/env bash
# One-shot demonstration: build, seed a legacy/target pair with known planted
# differences, verify the comparison reproduces them exactly, then start the
# dashboard.
set -euo pipefail
cd "$(dirname "$0")/.."

ROWS="${ROWS:-200000}"
DIR="${DIR:-./testdata}"

echo "==> building"
go build -o ./bin/bankcmp ./cmd/bankcmp
go build -o ./bin/seedtest ./cmd/seedtest

echo "==> seeding $DIR ($ROWS rows per table)"
./bin/seedtest -dir "$DIR" -rows "$ROWS"

echo "==> verifying the difference list against the planted manifest"
./bin/bankcmp compare -config "$DIR/config.yaml" -verify "$DIR/manifest.json"

echo
echo "==> starting the dashboard on http://127.0.0.1:8080 (Ctrl-C to stop)"
exec ./bin/bankcmp -addr 127.0.0.1:8080 -journal "$DIR/bankcmp.db"
