"""Requirements 4-5: distance annotations, dual readouts, accuracy bounds."""

import math

import pytest

from volview.errors import ErrorCode, VolViewError
from volview.measure import accuracy_bound, make_annotation
from volview.session import ReadingSession


def test_dual_readout_and_cross_check(model, store, volume_dir):
    from volview.model import VolumeRegistry
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m1")
    session.set_plane("axial", 50.0)
    ann = session.add_annotation([22.0, 32.0, 50.0], [42.0, 32.0, 50.0])
    reading = session.reading(ann.id)
    assert reading["world_length"] == pytest.approx(20.0)
    # voxel length: 20 mm along X with spacing 1.0 -> 20 voxels
    assert reading["voxel_length"] == pytest.approx(20.0)
    assert ann.cross_check(model)
    # voxel readout derives from world readout via spacing, per axis
    for pw, pv in zip(reading["endpoints_world"], reading["endpoints_voxel"]):
        assert pv[0] == pytest.approx(pw[0] / 1.0)
        assert pv[2] == pytest.approx(pw[2] / 2.5)


def test_voxel_length_uses_declared_spacing(model, store, volume_dir):
    from volview.model import VolumeRegistry
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m2")
    session.set_plane("sagittal", 32.0)  # normal X; in-plane (Z, Y)
    ann = session.add_annotation([32.0, 32.0, 45.0], [32.0, 32.0, 55.0])
    # 10 mm along Z with spacing 2.5 -> 4 voxels
    assert ann.world_length == pytest.approx(10.0)
    assert ann.voxel_length == pytest.approx(4.0)


def test_out_of_range_endpoint_identified(model, store, volume_dir):
    from volview.model import VolumeRegistry
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m3")
    session.set_plane("axial", 50.0)
    with pytest.raises(VolViewError) as exc:
        session.add_annotation([0.0, 0.0, 50.0], [1.0e6, 0.0, 50.0])
    assert exc.value.code == ErrorCode.E_RANGE
    assert exc.value.details["endpoint"] == 1
    assert exc.value.details["axis"] == "X"
    assert exc.value.details["annotation_id"] == 0


def test_annotation_must_lie_in_current_plane(model, store, volume_dir):
    from volview.model import VolumeRegistry
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m4")
    session.set_plane("axial", 50.0)
    with pytest.raises(VolViewError) as exc:
        session.add_annotation([22.0, 32.0, 40.0], [42.0, 32.0, 40.0])
    assert exc.value.code == ErrorCode.E_RANGE
    assert "plane" in str(exc.value)


def test_plane_switch_preserves_world_coords(model, store, volume_dir):
    from volview.model import VolumeRegistry
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m5")
    session.set_plane("axial", 50.0)
    ann = session.add_annotation([22.0, 32.0, 50.0], [42.0, 32.0, 50.0])
    before = [list(p) for p in ann.endpoints_world]
    # order-1 snapping moves y=32 to the half-voxel grid point 32.5
    snapped_y = before[0][1]
    assert snapped_y == pytest.approx(32.5)
    session.set_plane("coronal", snapped_y)
    projection = session.annotation_on_plane(ann.id)
    assert projection["endpoints_world"] == before
    assert projection["intersects"]  # segment lies in the y=32.5 plane
    # fractional pixel coords on coronal plane: rows=Z, cols=X
    # (x=22 snapped to 22.5 by the order-1 half-voxel grid)
    assert projection["pixels"][0] == pytest.approx([20.0, 22.5])
    # reading unchanged after switching planes
    assert ann.world_length == pytest.approx(20.0)


def test_accuracy_against_sphere_and_cylinder_truth(model, store,
                                                    volume_dir, truth):
    from volview.model import VolumeRegistry
    structures = truth("vol_a")["structures"]
    sphere = next(s for s in structures if s["kind"] == "sphere")
    cylinder = next(s for s in structures if s["kind"] == "cylinder")
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "m6")
    bound = accuracy_bound(model, session.interp_order)
    assert bound == pytest.approx(math.sqrt(3.0) * 1.25)  # max spacing 2.5 / 2

    # sphere diameter along X on the axial plane through the centre
    cx, cy, cz = sphere["center"]
    r = sphere["radius"]
    session.set_plane("axial", cz)
    ann = session.add_annotation([cx - r, cy, cz], [cx + r, cy, cz],
                                 true_length=2.0 * r)
    report = session.check_accuracy(ann.id, 2.0 * r)
    assert report["deviation"] <= report["bound"]
    assert report["bound"] == pytest.approx(bound)

    # cylinder axis length on the axial plane containing the axis
    p0, p1 = cylinder["axis_start"], cylinder["axis_end"]
    session.set_plane("axial", p0[2])
    ann2 = session.add_annotation(p0, p1,
                                  true_length=cylinder["analytic_lengths"]["axis_length"])
    report2 = session.check_accuracy(ann2.id,
                                     cylinder["analytic_lengths"]["axis_length"])
    assert report2["deviation"] <= report2["bound"]

    # cylinder diameter perpendicular to the axis (off-grid -> non-zero snap)
    mid = [(a + b) / 2 for a, b in zip(p0, p1)]
    d = cylinder["analytic_lengths"]["diameter"]
    ann3 = session.add_annotation([mid[0], mid[1] - d / 2, mid[2]],
                                  [mid[0], mid[1] + d / 2, mid[2]],
                                  true_length=d)
    report3 = session.check_accuracy(ann3.id, d)
    assert 0.0 <= report3["deviation"] <= report3["bound"]


def test_e_acc_when_deviation_exceeds_bound(model):
    with pytest.raises(VolViewError) as exc:
        make_annotation(model, 7, "axial", 50.0, [22.0, 32.0, 50.0],
                        [42.0, 32.0, 50.0], order=1, true_length=100.0)
    assert exc.value.code == ErrorCode.E_ACC
    assert exc.value.details["annotation_id"] == 7
    assert exc.value.details["measured"] == pytest.approx(20.0)
    assert exc.value.details["bound"] == pytest.approx(math.sqrt(3.0) * 1.25)
