import ast
import copy
import json
import pathlib
import shutil

import pytest

from syncdemo import runner, truth
from syncdemo.__main__ import main
from syncdemo.errors import DivergeError


def test_truth_module_is_independent_of_engine_client_runner():
    tree = ast.parse(
        pathlib.Path(truth.__file__).read_text(encoding="utf-8")
    )
    imported = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom):
            imported.add(node.module or "")
        elif isinstance(node, ast.Import):
            imported.update(alias.name for alias in node.names)
    forbidden = {"engine", "client", "runner",
                 "syncdemo.engine", "syncdemo.client", "syncdemo.runner"}
    assert not (forbidden & imported)


def test_independent_truth_matches_runner(sample):
    result = runner.run_replay(sample)
    runner.compare_truth(result, sample.truth)  # must not raise


def test_key_tick_state_mismatch_is_localised(sample):
    result = runner.run_replay(sample)
    truth_doc = copy.deepcopy(sample.truth)
    entry = truth_doc["key_states"][2]
    uid = sorted(entry["units"])[0]
    entry["units"][uid]["hp"] += 1.0
    with pytest.raises(DivergeError) as exc:
        runner.compare_truth(result, truth_doc)
    assert exc.value.code == "E_DIVERGE"
    assert exc.value.tick == entry["tick"]
    assert exc.value.unit == uid
    assert exc.value.field == "hp"


def test_client_visible_mismatch_is_localised(sample):
    result = runner.run_replay(sample)
    truth_doc = copy.deepcopy(sample.truth)
    entry = truth_doc["client_visible"][0]
    uid = sorted(entry["units"])[0]
    entry["units"][uid]["resource"] += 1.0
    with pytest.raises(DivergeError) as exc:
        runner.compare_truth(result, truth_doc)
    assert exc.value.tick == entry["tick"]
    assert exc.value.unit == uid
    assert exc.value.field == "resource"
    assert entry["client"] in str(exc.value)


def test_settlement_mismatch_is_detected(sample):
    result = runner.run_replay(sample)
    truth_doc = copy.deepcopy(sample.truth)
    pid = sorted(truth_doc["settlement"]["players"])[0]
    truth_doc["settlement"]["players"][pid]["hp"] += 1.0
    with pytest.raises(DivergeError) as exc:
        runner.compare_truth(result, truth_doc)
    assert exc.value.field == "settlement"


def test_verify_command_fails_on_truth_mismatch(sample_dir, tmp_path, capsys):
    work_sample = tmp_path / "sample"
    shutil.copytree(sample_dir, work_sample)
    truth_path = work_sample / "truth.json"
    doc = json.loads(truth_path.read_text(encoding="utf-8"))
    entry = doc["key_states"][1]
    uid = sorted(entry["units"])[0]
    entry["units"][uid]["hp"] += 1.0
    truth_path.write_text(json.dumps(doc), encoding="utf-8")

    rc = main(["verify", "--sample", str(work_sample),
               "--work", str(tmp_path / "work")])
    assert rc == 2
    assert "E_DIVERGE" in capsys.readouterr().err
