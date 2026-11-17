import json
import os

import pytest
import soundfile as sf

from applause_master.errors import BusinessError
from applause_master.project import load_project


def _copy_project(demo_workspace, tmp_path):
    with open(demo_workspace["project_path"]) as handle:
        data = json.load(handle)
    base = os.path.dirname(demo_workspace["project_path"])
    target = tmp_path / "project.json"
    return data, base, str(target)


def _write_and_expect(code, demo_workspace, tmp_path, mutate):
    data, base, target = _copy_project(demo_workspace, tmp_path)
    # Re-point mic files to the existing synthesized absolute paths.
    for mic in data["microphones"]:
        mic["file"] = os.path.join(base, mic["file"])
    mutate(data, tmp_path)
    with open(target, "w") as handle:
        json.dump(data, handle)
    with pytest.raises(BusinessError) as exc:
        load_project(target)
    assert exc.value.code == code
    return exc.value


def test_missing_file_rejected(demo_workspace, tmp_path):
    err = _write_and_expect("FILE_MISSING", demo_workspace, tmp_path,
                            lambda d, p: d["microphones"][0].update(file=str(p / "nope.wav")))
    assert err.details["mic"] == demo_workspace["project"]["microphones"][0]["name"]


def test_sample_rate_mismatch_rejected(demo_workspace, tmp_path):
    def mutate(data, path):
        mic = data["microphones"][0]
        y, sr = sf.read(mic["file"])
        sf.write(str(path / "rewrite.wav"), y, 24000)
        mic["file"] = str(path / "rewrite.wav")
    err = _write_and_expect("SAMPLE_RATE_MISMATCH", demo_workspace, tmp_path, mutate)
    assert err.details["actual"] == 24000


def test_channel_count_mismatch_rejected(demo_workspace, tmp_path):
    def mutate(data, path):
        mic = data["microphones"][1]
        y, sr = sf.read(mic["file"])
        stereo = np_repeat(y)
        sf.write(str(path / "stereo.wav"), stereo, sr)
        mic["file"] = str(path / "stereo.wav")
    err = _write_and_expect("CHANNEL_COUNT_MISMATCH", demo_workspace, tmp_path, mutate)
    assert err.details["actual"] == 2


def np_repeat(y):
    import numpy as np
    return np.repeat(y.reshape(-1, 1), 2, axis=1)


def test_duplicate_mic_name_rejected(demo_workspace, tmp_path):
    _write_and_expect(
        "DUPLICATE_MIC_NAME", demo_workspace, tmp_path,
        lambda d, p: d["microphones"][1].update(name=d["microphones"][0]["name"]),
    )


def test_overlapping_band_rejected(demo_workspace, tmp_path):
    def mutate(data, path):
        data["freq_bands"][1]["low_hz"] = data["freq_bands"][0]["low_hz"]
    err = _write_and_expect("INVALID_FREQ_BAND", demo_workspace, tmp_path, mutate)
    assert "band" in err.details


def test_band_above_nyquist_rejected(demo_workspace, tmp_path):
    def mutate(data, path):
        data["freq_bands"][-1]["high_hz"] = 30000.0
    _write_and_expect("INVALID_FREQ_BAND", demo_workspace, tmp_path, mutate)


def test_too_few_bands_rejected(demo_workspace, tmp_path):
    _write_and_expect("INVALID_FREQ_BAND", demo_workspace, tmp_path,
                      lambda d, p: d.update(freq_bands=d["freq_bands"][:2]))


def test_cli_rejects_with_nonzero_exit(demo_workspace, tmp_path):
    import subprocess
    import sys
    data, base, target = _copy_project(demo_workspace, tmp_path)
    for mic in data["microphones"]:
        mic["file"] = os.path.join(base, mic["file"])
    data["microphones"][0]["file"] = str(tmp_path / "missing.wav")
    with open(target, "w") as handle:
        json.dump(data, handle)
    proc = subprocess.run(
        [sys.executable, "-m", "applause_master.cli", "render", target,
         "--output", str(tmp_path / "out"), "--no-interrupt"],
        capture_output=True, text=True,
    )
    assert proc.returncode != 0
    payload = json.loads(proc.stderr)
    assert payload["code"] == "FILE_MISSING"
    assert not os.path.exists(os.path.join(str(tmp_path / "out"), "master_final.wav"))
