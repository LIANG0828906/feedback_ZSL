"""细分控制网（Catmull-Clark 极限点）解析真值校验。"""
import numpy as np


def _cc_limit(vertices: np.ndarray, size: int, vertex: int) -> np.ndarray:
    i, j = divmod(vertex, size)
    neigh = [vertex - size, vertex + size, vertex - 1, vertex + 1]
    return (vertices[vertex] + 0.25 * vertices[neigh].sum(axis=0)) / 2.0


def test_subdivision_limit_points_match_truth(workdir):
    from cadrender.loader import load_scene
    _, meshes, _ = load_scene(workdir)
    assert meshes, "应至少加载一个细分控制网"
    for mesh in meshes:
        verts = np.asarray(mesh["vertices"], dtype=np.float64)
        size = int(round(np.sqrt(len(verts))))
        for rec in mesh["truth_limit_points"]:
            expect = _cc_limit(verts, size, rec["vertex"])
            np.testing.assert_allclose(expect, rec["limit_point"],
                                       atol=1e-9)
