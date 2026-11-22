"""需求 8(组件接口): 纯函数式查询, 不影响内部状态。"""

import os

import pytest

from a11y_media.api import MediaAccessibilityService
from a11y_media.errors import ComponentError


@pytest.fixture(scope="module")
def service(samples_dir, tmp_path_factory):
    proj_dir = os.path.join(samples_dir, sorted(os.listdir(samples_dir))[0])
    svc = MediaAccessibilityService(
        proj_dir, checkpoint_root=str(tmp_path_factory.mktemp("cp"))
    )
    svc.run_realtime()
    return svc


def test_get_window_matches_truth(service):
    for i, (t0, t1) in enumerate(service.project.truth.windows):
        w0, w1 = service.get_window(i)
        tol = service.project.tolerance_samples
        assert abs(w0 - t0) <= tol and abs(w1 - t1) <= tol


def test_get_char_interval_tiling(service):
    for sub_id in range(len(service.project.subtitles)):
        w0, w1 = service.get_window(sub_id)
        n = len(service.project.subtitles[sub_id].text)
        prev_end = w0
        for c in range(n):
            s, e = service.get_char_interval(sub_id, c)
            assert s == prev_end and s < e
            prev_end = e
        assert prev_end == w1


def test_queries_do_not_mutate(service):
    before = service.get_error_report()
    service.get_window(0)
    service.get_char_interval(0, 0)
    service.get_error_report()
    assert service.get_error_report() == before


def test_query_out_of_range(service):
    with pytest.raises(ComponentError):
        service.get_window(10**6)
    with pytest.raises(ComponentError):
        service.get_char_interval(0, 10**6)


def test_error_report_export_shape(service):
    report = service.get_error_report()
    assert report["time_unit"] == "samples"
    assert len(report["sentences"]) == len(service.project.subtitles)
    assert report["total_read"]["deviation"] <= report["total_read"]["bound"]
