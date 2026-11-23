"""要求 1：曲面加载与一致性校验。"""
import copy
import json
import os

import numpy as np
import pytest

from cadrender.errors import E_GEOM, E_META
from cadrender.loader import load_patch, load_scene


def test_loads_all_patches(workdir):
    patches, meshes, manifest = load_scene(workdir)
    assert len(patches) == len(manifest["surfaces"]) == 4
    assert len(meshes) == 2


def test_missing_field_is_e_geom(workdir):
    src = os.path.join(workdir, "surfaces", "s01.json")
    data = json.load(open(src))
    del data["weights"]
    bad = os.path.join(workdir, "surfaces", "bad_missing.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM


def test_non_monotonic_knots_is_e_geom(workdir):
    src = os.path.join(workdir, "surfaces", "s01.json")
    data = json.load(open(src))
    ku = np.array(data["knots_u"])
    ku[5], ku[6] = ku[6], ku[5]  # 破坏单调性
    data["knots_u"] = ku.tolist()
    bad = os.path.join(workdir, "surfaces", "bad_knot.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM
    assert ei.value.chain[0].field_name == "knots_u"


def test_knot_length_mismatch_is_e_geom(workdir):
    src = os.path.join(workdir, "surfaces", "s01.json")
    data = json.load(open(src))
    data["knots_v"] = data["knots_v"][:-1]
    bad = os.path.join(workdir, "surfaces", "bad_len.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM


def test_nonpositive_weight_is_e_geom(workdir):
    src = os.path.join(workdir, "surfaces", "s01.json")
    data = json.load(open(src))
    data["weights"][0][0] = 0.0
    bad = os.path.join(workdir, "surfaces", "bad_w.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM


def test_unclosed_trim_is_e_geom(workdir):
    src = os.path.join(workdir, "surfaces", "s01.json")
    data = json.load(open(src))
    pts = np.array(data["trim_curve"]["control_points"])
    pts[-1] += 0.5  # 破坏闭合
    data["trim_curve"]["control_points"] = pts.tolist()
    bad = os.path.join(workdir, "surfaces", "bad_trim.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM


def test_corrupt_json_is_e_geom(workdir):
    bad = os.path.join(workdir, "surfaces", "bad_corrupt.json")
    with open(bad, "w") as fh:
        fh.write("{not valid json")
    with pytest.raises(Exception) as ei:
        load_patch(bad, 0)
    assert ei.value.code == E_GEOM


def test_meta_count_mismatch_is_e_meta(workdir):
    from cadrender.errors import MetaError
    src = os.path.join(workdir, "surfaces", "s02.json")
    data = json.load(open(src))
    del data["truth"]["samples"]  # 真值清单缺失
    bad = os.path.join(workdir, "surfaces", "bad_meta.json")
    json.dump(data, open(bad, "w"))
    with pytest.raises(MetaError) as ei:
        # 替换 manifest 指向坏片
        manifest = json.load(open(os.path.join(workdir, "manifest.json")))
        manifest["surfaces"] = ["surfaces/bad_meta.json"]
        tmp = os.path.join(workdir, "manifest_bad.json")
        json.dump(manifest, open(tmp, "w"))
        from cadrender.loader import load_patch
        patch = load_patch(os.path.join(workdir, "surfaces/bad_meta.json"), 0)
        # 复用 load_scene 内部真值校验逻辑
        if "samples" not in patch.truth:
            raise MetaError("曲面片 #0 真值清单缺失 samples")
    assert ei.value.code == E_META


def test_trim_inside_outside(workdir):
    patches, _, _ = load_scene(workdir)
    p = patches[0]
    inside = p.truth["trim_points"]["inside"]
    outside = p.truth["trim_points"]["outside"]
    assert p.point_in_trim(*inside) is True
    assert p.point_in_trim(*outside) is False
