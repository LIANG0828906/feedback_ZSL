"""NURBS 曲面与曲线的确定性求值。

采用 Cox–de Boor 递推计算 B 样条基函数；有理曲面通过齐次坐标
``A = sum B * w * [P, 1]`` 求值，其一阶偏导精确给出有理偏导
``dS = (dA_xyz - S * dA_w) / A_w``，再由偏导叉积得到解析法线。
所有实现均为纯 numpy、无随机量，相同输入逐字节级可复现。
"""
from __future__ import annotations

from dataclasses import dataclass
from typing import Tuple

import numpy as np


def find_span(n: int, degree: int, u: float, knots: np.ndarray) -> int:
    """返回参数 u 所在的节点区间索引。"""
    if u >= knots[n + 1]:
        return n
    if u <= knots[degree]:
        return degree
    low, high = degree, n + 1
    mid = (low + high) // 2
    while u < knots[mid] or u >= knots[mid + 1]:
        if u < knots[mid]:
            high = mid
        else:
            low = mid
        mid = (low + high) // 2
    return mid


def basis_functions(span: int, u: float, degree: int,
                    knots: np.ndarray) -> np.ndarray:
    """非零 B 样条基函数值，长度 degree+1。"""
    n = np.empty(degree + 1, dtype=np.float64)
    n[0] = 1.0
    left = np.empty(degree + 1, dtype=np.float64)
    right = np.empty(degree + 1, dtype=np.float64)
    for j in range(1, degree + 1):
        left[j] = u - knots[span + 1 - j]
        right[j] = knots[span + j] - u
        saved = 0.0
        for r in range(j):
            tmp = n[r] / (right[r + 1] + left[j - r])
            n[r] = saved + right[r + 1] * tmp
            saved = left[j - r] * tmp
        n[j] = saved
    return n


def basis_ders(span: int, u: float, degree: int, n_ders: int,
               knots: np.ndarray) -> np.ndarray:
    """基函数导数，返回形状 (n_ders+1, degree+1)。"""
    ndu = np.zeros((degree + 1, degree + 1), dtype=np.float64)
    ndu[0, 0] = 1.0
    left = np.empty(degree + 1, dtype=np.float64)
    right = np.empty(degree + 1, dtype=np.float64)
    for j in range(1, degree + 1):
        left[j] = u - knots[span + 1 - j]
        right[j] = knots[span + j] - u
        saved = 0.0
        for r in range(j):
            ndu[j, r] = right[r + 1] + left[j - r]
            tmp = ndu[r, j - 1] / ndu[j, r]
            ndu[r, j] = saved + right[r + 1] * tmp
            saved = left[j - r] * tmp
        ndu[j, j] = saved
    ders = np.zeros((n_ders + 1, degree + 1), dtype=np.float64)
    for j in range(degree + 1):
        ders[0, j] = ndu[j, degree]
    a = np.zeros((2, degree + 1), dtype=np.float64)
    for r in range(degree + 1):
        s1, s2 = 0, 1
        a[0, 0] = 1.0
        for k in range(1, n_ders + 1):
            d = 0.0
            rk = r - k
            pk = degree - k
            if r >= k:
                a[s2, 0] = a[s1, 0] / ndu[pk + 1, rk]
                d = a[s2, 0] * ndu[rk, pk]
            j1 = 1 if rk >= -1 else -rk
            j2 = k - 1 if r - 1 <= pk else degree - r
            for j in range(j1, j2 + 1):
                a[s2, j] = (a[s1, j] - a[s1, j - 1]) / ndu[pk + 1, rk + j]
                d += a[s2, j] * ndu[rk + j, pk]
            if r <= pk:
                a[s2, k] = -a[s1, k - 1] / ndu[pk + 1, r]
                d += a[s2, k] * ndu[r, pk]
            ders[k, r] = d
            s1, s2 = s2, s1
    fac = 1.0
    for k in range(1, n_ders + 1):
        fac *= k
        ders[k] *= fac
    return ders


@dataclass
class NurbsSurface:
    control_points: np.ndarray   # (n+1, m+1, 3)
    weights: np.ndarray          # (n+1, m+1)
    knots_u: np.ndarray
    knots_v: np.ndarray
    degree_u: int
    degree_v: int
    name: str = "surface"

    @property
    def n(self) -> int:
        return self.control_points.shape[0] - 1

    @property
    def m(self) -> int:
        return self.control_points.shape[1] - 1

    def _homogeneous(self, u: float, v: float) -> np.ndarray:
        """返回 3x4 齐次表示：行 0/1/2 为 A、dA/du、dA/dv。"""
        su = find_span(self.n, self.degree_u, u, self.knots_u)
        sv = find_span(self.m, self.degree_v, v, self.knots_v)
        du = basis_ders(su, u, self.degree_u, 1, self.knots_u)
        dv = basis_ders(sv, v, self.degree_v, 1, self.knots_v)
        nu, dnu = du[0], du[1]
        nv, dnv = dv[0], dv[1]
        i0 = su - self.degree_u
        j0 = sv - self.degree_v
        pw = self.control_points[i0:su + 1, j0:sv + 1]      # (p+1,q+1,3)
        ww = self.weights[i0:su + 1, j0:sv + 1]             # (p+1,q+1)
        out = np.zeros((3, 4), dtype=np.float64)
        for k in range(3):
            if k == 0:
                bu, bv = nu, nv
            elif k == 1:
                bu, bv = dnu, nv
            else:
                bu, bv = nu, dnv
            for i in range(pw.shape[0]):
                for j in range(pw.shape[1]):
                    coef = bu[i] * bv[j]
                    w = ww[i, j]
                    out[k, :3] += coef * w * pw[i, j]
                    out[k, 3] += coef * w
        return out

    def eval_ders(self, u: float, v: float) -> Tuple[np.ndarray, np.ndarray,
                                                      np.ndarray, np.ndarray]:
        """返回 (点, dS/du, dS/dv, 单位法线)。"""
        a = self._homogeneous(u, v)
        w0 = a[0, 3]
        point = a[0, :3] / w0
        du = (a[1, :3] - point * a[1, 3]) / w0
        dv = (a[2, :3] - point * a[2, 3]) / w0
        normal = np.cross(du, dv)
        norm = np.linalg.norm(normal)
        if norm > 1e-12:
            normal = normal / norm
        return point, du, dv, normal

    def point(self, u: float, v: float) -> np.ndarray:
        return self.eval_ders(u, v)[0]

    def control_hull_bbox(self) -> np.ndarray:
        return np.stack([self.control_points.min(axis=(0, 1)),
                         self.control_points.max(axis=(0, 1))])


@dataclass
class NurbsCurve2D:
    """参数域 (u,v) 内的 NURBS 裁剪曲线，控制点形状 (k+1, 2)。"""

    control_points: np.ndarray
    weights: np.ndarray
    knots: np.ndarray
    degree: int

    @property
    def n(self) -> int:
        return self.control_points.shape[0] - 1

    def point(self, t: float) -> np.ndarray:
        span = find_span(self.n, self.degree, t, self.knots)
        nfun = basis_functions(span, t, self.degree, self.knots)
        i0 = span - self.degree
        pts = self.control_points[i0:span + 1]
        w = self.weights[i0:span + 1]
        denom = float(np.sum(nfun * w))
        num = np.sum((nfun * w)[:, None] * pts, axis=0)
        return num / denom

    def polyline(self, samples: int = 64) -> np.ndarray:
        ts = np.linspace(float(self.knots[self.degree]),
                         float(self.knots[self.n + 1]), samples)
        return np.stack([self.point(float(t)) for t in ts])
