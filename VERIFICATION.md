# Verification record

Environment: Linux/WSL Ubuntu, Go 1.22.12, `CGO_ENABLED=1` but the implementation
does not use CGO. No third-party Go modules are required.

## Commands

```sh
go test ./...
CGO_ENABLED=0 go test ./...
go vet ./...
go build -o /tmp/registryctl ./cmd/registryctl
/tmp/registryctl gen --out samples --seed 42
/tmp/registryctl compare --samples samples --seed 42
/tmp/registryctl verify --seed 42
```

## Actual results

- `go test ./...`: all packages pass.
- `CGO_ENABLED=0 go test ./...`: all packages pass.
- `go vet ./...`: no findings.
- Generated samples:
  - v1: 105226 bytes
  - v2: 105118 bytes
  - v3: 106381 bytes
  - normalized model hash for all three:
    `b7dfa9436d2d73ef72571015811f73f2818a02d2ee2c78132d9d36ce88d3f5d3`
  - 21 logical ticks, 5 health transitions, 10 query results, 1 deferred
    probe snapshot.
- `compare`: all three versions matched the embedded truth and each other.
- `verify`:
  - three-schema normalization and truth: PASS
  - incremental probe-period edit (reused 10, recomputed 11): PASS
  - checkpoint resume from tick 7: PASS
  - SIGKILL after commit tick 5, resume to full timeline: PASS
  - eleven anomaly cases, all stable codes/context/immutability: PASS
- Independent SQLite validation:
  - `file checkpoint.db` reports `SQLite 3.x database`.
  - Python `sqlite3` reading the same file reports `PRAGMA auto_vacuum = 0`
    and `PRAGMA integrity_check = ok`.
- Graceful SIGTERM run:
  - checkpoint commit `3`, next tick `4`;
  - process exit code `143`;
  - checkpoint loaded successfully after interruption.

The exact `verify` JSON can be regenerated with:

```sh
/tmp/registryctl verify --seed 42
```
