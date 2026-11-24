#!/usr/bin/env bash
set -euo pipefail

export PATH="$PATH:$(go env GOROOT)/bin"
export CGO_ENABLED=0

SEED="${SEED:-420261125}"
DB="artifacts/acceptance-crash-$(date +%s).sqlite"

go test ./...
go run ./cmd/samplegen -out samples -seed "${SEED}"
go run ./cmd/cadpipe verify -seed "${SEED}" \
  --report artifacts/verification-report.json \
  --work artifacts/acceptance
GOOS=windows GOARCH=amd64 go build ./...
go build -o artifacts/cadpipe ./cmd/cadpipe

set +e
./artifacts/cadpipe crash --db "${DB}" --after 5 --seed "${SEED}" \
  > artifacts/acceptance-crash.log 2>&1
code=$?
set -e
if [[ "${code}" -ne 137 ]]; then
  echo "expected SIGKILL-style exit 137, got ${code}" >&2
  exit 1
fi

./artifacts/cadpipe recover --db "${DB}" --seed "${SEED}" \
  | tee artifacts/acceptance-recover.log
echo "acceptance passed; crash db=${DB}"
