#!/usr/bin/env bash
# Scripted, network-free forensic demo. All inputs are generated locally;
# repeated runs with the same seed produce byte-identical trajectory logs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/env.sh"
BUILD="$ROOT/build"
DEMO="$ROOT/artifacts/demo"

echo "== [0/8] preparing deterministic samples =="
mkdir -p "$DEMO"
"$BUILD/vf_generate" --config "$ROOT/config/sample_profiles.json" --out "$DEMO/samples"

run_case() {
  local name="$1" manifest="$2" extra=("${@:3}")
  local wd="$DEMO/workdirs/$name"
  mkdir -p "$wd"
  echo "== verifying $name =="
  "$BUILD/vf_cli" verify \
    --sequence "$DEMO/samples/$name.vfseq" \
    --manifest "$manifest" \
    --reference "$DEMO/samples/$name.vref" \
    --workdir "$wd" \
    --clock 1700000000000 \
    --report "$wd/diagnostic_report.json" \
    --trajectory "$wd/trajectory.log" \
    "${extra[@]}"
}

echo "== [1/8] clean sequence: all frames trusted =="
run_case small_clean "$DEMO/samples/small_clean.manifest.json"

echo "== [2/8] re-encoded sequence: mismatch + downstream cascade =="
run_case medium_reencode "$DEMO/samples/medium_reencode.manifest.json"

echo "== [3/8] second run must reuse every cached frame =="
"$BUILD/vf_cli" verify \
  --sequence "$DEMO/samples/medium_reencode.vfseq" \
  --manifest "$DEMO/samples/medium_reencode.manifest.json" \
  --reference "$DEMO/samples/medium_reencode.vref" \
  --workdir "$DEMO/workdirs/medium_reencode" \
  --clock 1700000000001 \
  --trajectory "$DEMO/workdirs/medium_reencode/trajectory2.log"

echo "== [4/8] manifest v1 -> v2 upgrade with anchor semantic diff =="
# Publish a v1 baseline into the upgrade workdir first, then re-verify the
# same pixels against the v2 manifest so anchor semantics are diffed.
upgrade_wd="$DEMO/workdirs/medium_v2_upgrade"
mkdir -p "$upgrade_wd"
"$BUILD/vf_cli" verify \
  --sequence "$DEMO/samples/medium_v2_upgrade.vfseq" \
  --manifest "$DEMO/samples/medium_reencode.manifest.json" \
  --reference "$DEMO/samples/medium_v2_upgrade.vref" \
  --workdir "$upgrade_wd" --clock 1700000000000 >/dev/null
"$BUILD/vf_cli" verify \
  --sequence "$DEMO/samples/medium_v2_upgrade.vfseq" \
  --manifest "$DEMO/samples/medium_v2_upgrade.manifest.json" \
  --reference "$DEMO/samples/medium_v2_upgrade.vref" \
  --workdir "$upgrade_wd" --clock 1700000000001 \
  --report "$upgrade_wd/diagnostic_report.json" \
  --trajectory "$upgrade_wd/trajectory.log"

echo "== [5/8] snapshot recovery: resume from newest valid snapshot =="
"$BUILD/vf_cli" resume \
  --sequence "$DEMO/samples/medium_reencode.vfseq" \
  --manifest "$DEMO/samples/medium_reencode.manifest.json" \
  --reference "$DEMO/samples/medium_reencode.vref" \
  --workdir "$DEMO/workdirs/medium_reencode" \
  --clock 1700000000002

echo "== [6/8] duplicate submission returns E_BUSY, cancel preserves snapshot =="
"$BUILD/vf_cli" busy-test \
  --sequence "$DEMO/samples/medium_reencode.vfseq" \
  --manifest "$DEMO/samples/medium_reencode.manifest.json" \
  --reference "$DEMO/samples/medium_reencode.vref" \
  --workdir "$DEMO/workdirs/medium_reencode"

echo "== [7/8] GUI offscreen smoke test (reads published snapshot only) =="
QT_QPA_PLATFORM=offscreen timeout 5 "$BUILD/vf_gui" \
  --workdir "$DEMO/workdirs/medium_reencode" \
  --sequence "$DEMO/samples/medium_reencode.vfseq" >/dev/null 2>&1 || true

echo "== [8/8] determinism: regenerate samples and diff trajectory logs =="
"$BUILD/vf_generate" --config "$ROOT/config/sample_profiles.json" --out "$DEMO/samples_rerun"
for name in small_clean medium_reencode medium_v2_upgrade; do
  "$BUILD/vf_cli" verify \
    --sequence "$DEMO/samples_rerun/$name.vfseq" \
    --manifest "$DEMO/samples_rerun/$name.manifest.json" \
    --reference "$DEMO/samples_rerun/$name.vref" \
    --workdir "$DEMO/workdirs/${name}_rerun" \
    --clock 1700000000000 \
    --trajectory "$DEMO/workdirs/${name}_rerun/trajectory.log" >/dev/null
  if cmp -s "$DEMO/workdirs/$name/trajectory.log" "$DEMO/workdirs/${name}_rerun/trajectory.log"; then
    echo "  deterministic OK: $name"
  else
    echo "  DETERMINISM FAILURE: $name"; diff "$DEMO/workdirs/$name/trajectory.log" \
      "$DEMO/workdirs/${name}_rerun/trajectory.log"; exit 1
  fi
done

echo
echo "All demo stages completed. Artifacts under: $DEMO"
