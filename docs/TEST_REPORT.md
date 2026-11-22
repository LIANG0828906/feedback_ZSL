# Test Report

## Environment

- Host workspace: WSL2 Ubuntu 26.04 on Windows
- Python: 3.14.4
- NumPy: 2.5.5
- PySide6: 6.12.0
- `<workspace>/.venv`: isolated virtual environment created for this task
- Network: used only once to install the two requested dependencies; sample
  generation and all tests use local files only
- Container/Docker: not used

The WSL image did not include `ensurepip` or `python3.14-venv`, and the session
had no passwordless `sudo`. The isolated environment was therefore created with
`python3 -m venv --without-pip .venv`, followed by the official `get-pip.py`
bootstrapper inside that virtual environment.

The container cannot invoke the Windows host (`cmd.exe` and `powershell.exe`
are unavailable), so Windows execution was not performed here. The code uses
`pathlib`, `os.replace`, `msvcrt` locking on Windows and `fcntl` locking on
POSIX; Windows setup steps are documented in the README.

## Commands

```bash
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python -m vp_pipeline.cli verify --samples samples
QT_QPA_PLATFORM=offscreen .venv/bin/python -m unittest tests.test_ui -v
```

## Recorded Results

### Full suite

```text
Ran 37 tests in 1.231s
OK
```

Coverage includes loader/error paths including inconsistent premultiplied RGB,
independent sample truth, deterministic render repetition, state transitions,
incremental invalidation, diff localization, checkpoint recovery/tamper
rejection, edited-model recovery only after all window snapshots match,
serial/parallel equality, failure isolation/retry, 3x3 export interoperability,
byte-stable repeated export, and the PySide6 offscreen operator workflow.

### End-to-end verification

Command:

```bash
.venv/bin/python -m vp_pipeline.cli verify --samples samples
```

Recorded outcomes:

| Check | Result |
| --- | --- |
| Loaded versions | `vpfc/1`, `vpfc/2`, `vpfc/3` |
| Mapping entries | 67 / 77 / 71 |
| Frame instructions | 18 frames, 108 sample coordinates validated on all versions |
| Reference pixel checks | 324 passed |
| Cross-version frame equality | true |
| Incremental edit | `prop_crate.opacity = 0.41` |
| Invalidated/recomputed windows | 0, 1, 2 |
| Reused windows | 3, 4, 5 |
| Incremental vs full pixels | equal |
| Parallel vs serial | equal frames and commit order |
| Initial commit order | 0, 1, 2, 3, 4, 5 |
| Incremental commit order | 0, 1, 2 |
| Export combinations | 9, all pixel-equal after readback |
| Lossy warnings | 15 total; `vpfc/2→1` = 6, `vpfc/3→1` = 9 |
| Restart recovery | windows 0–5 restored |
| Tampered checkpoint | whole snapshot discarded, progress 0/6 |

### Deterministic regeneration

```bash
.venv/bin/python scripts/generate_samples.py \
  --seed 20261123 --output /tmp/vp_samples_check
diff -rq --exclude README.md samples /tmp/vp_samples_check
```

Result: all seven generated files are byte-identical to the committed samples;
the manifest hash verification also passed. `samples/README.md` is hand-written
documentation and is intentionally not emitted by the generator.

### Compilation

```text
python -m compileall -q vp_pipeline scripts tests
COMPILE_OK
```

### One-command acceptance

```bash
QT_QPA_PLATFORM=offscreen .venv/bin/python scripts/run_acceptance.py
```

Result: exit code `0`; it reran the full unit suite and the end-to-end
verification above.
