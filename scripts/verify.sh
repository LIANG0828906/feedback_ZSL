#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

cmake -S . -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j "${JOBS:-4}"
cmake --build build --target check

cmake -E make_directory build/verify
cmake -E remove_directory build/verify/run_a
cmake -E remove_directory build/verify/run_b
cmake -E remove_directory build/verify/run_same
cmake -E remove_directory build/verify/run_view_b

./build/scene_gen --out-dir data
./build/shadowpipe --scene data/synthetic.rscene --config data/sample.script \
    --output build/verify/run_a
./build/shadowpipe --scene data/synthetic.rscene --config data/sample.script \
    --output build/verify/run_b

(cd data/baseline && sha256sum -c SHA256SUMS)
for file in shadow_factor.pgm shadow_factor.f32 visibility_mask.pgm \
            error_heatmap.ppm summary.json snapshot.bin; do
    if ! cmp -s "data/baseline/${file}" "build/verify/run_a/${file}"; then
        echo "baseline mismatch: ${file}" >&2
        exit 1
    fi
    if ! cmp -s "build/verify/run_a/${file}" "build/verify/run_b/${file}"; then
        echo "determinism failure: ${file}" >&2
        exit 1
    fi
done
echo "baseline and determinism: byte-identical"

./build/shadowpipe --scene data/synthetic.rscene --config data/sample.script \
    --output build/verify/run_same --load-snapshot build/verify/run_a/snapshot.bin
if ! cmp -s build/verify/run_a/summary.json build/verify/run_same/summary.json; then
    echo "snapshot-stable summary differs" >&2
    exit 1
fi
echo "snapshot unchanged: no rerender differences"

./build/shadowpipe --scene data/synthetic.rscene --config data/view_b.script \
    --output build/verify/run_view_b --load-snapshot build/verify/run_a/snapshot.bin
python3 - <<'PY'
import json
summary = json.load(open("build/verify/run_view_b/summary.json", encoding="utf-8"))
version = summary["snapshot_version"]
assert version["changed"] and version["view_changed"]
assert version["cascade_table_changed"]
print("snapshot changed: view/cascade rerender detected")
PY

cp build/verify/run_a/snapshot.bin build/verify/corrupt.snapshot
python3 - <<'PY'
path = "build/verify/corrupt.snapshot"
with open(path, "r+b") as stream:
    stream.seek(64)
    value = stream.read(1)
    stream.seek(64)
    stream.write(bytes([value[0] ^ 0x5a]))
PY
set +e
./build/shadowpipe --scene data/synthetic.rscene --config data/sample.script \
    --output build/verify/run_corrupt \
    --load-snapshot build/verify/corrupt.snapshot >/dev/null 2>&1
code="$?"
set -e
if [[ "${code}" -ne 13 ]]; then
    echo "expected E_SNAPSHOT_MISMATCH exit 13, got ${code}" >&2
    exit 1
fi
echo "snapshot corruption: E_SNAPSHOT_MISMATCH"
echo "all verification checks passed"

