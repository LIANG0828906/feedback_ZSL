"""三路径一致性比对: 实时 / 断点恢复 / 全量重算。

逐句比对显示窗口、逐字符比对高亮起始时刻、比对误差报告统计;
发现首个不一致即抛出 E_DIVERGE, 输出句号、字符序号、字段名与两路径取值。
"""

from __future__ import annotations

from .engine import ProjectResult
from .errors import diverge_error


def first_divergence(a: ProjectResult, b: ProjectResult) -> dict | None:
    """返回首个不一致位置, 完全一致返回 None。"""
    if len(a.windows) != len(b.windows):
        return {"sub": -1, "char": None, "field": "num_windows",
                "path_a": len(a.windows), "path_b": len(b.windows)}
    for i, (wa, wb) in enumerate(zip(a.windows, b.windows)):
        if tuple(wa) != tuple(wb):
            return {"sub": i, "char": None, "field": "window",
                    "path_a": list(wa), "path_b": list(wb)}
    for i, (sa, sb) in enumerate(zip(a.char_starts, b.char_starts)):
        if len(sa) != len(sb):
            return {"sub": i, "char": None, "field": "num_chars",
                    "path_a": len(sa), "path_b": len(sb)}
        for j, (ta, tb) in enumerate(zip(sa, sb)):
            if ta != tb:
                return {"sub": i, "char": j, "field": "char_start",
                        "path_a": ta, "path_b": tb}
    ra, rb = a.report, b.report
    ea = {e["sub"]: e for e in ra.get("sentences", [])}
    eb = {e["sub"]: e for e in rb.get("sentences", [])}
    for sub in sorted(ea):
        if ea[sub] != eb.get(sub):
            fa, fb = ea[sub], eb.get(sub, {})
            for key in fa:
                if fa[key] != fb.get(key):
                    return {"sub": sub, "char": None, "field": f"report.{key}",
                            "path_a": fa[key], "path_b": fb.get(key)}
    for key in ("bounds", "total_read"):
        if ra.get(key) != rb.get(key):
            return {"sub": -1, "char": None, "field": f"report.{key}",
                    "path_a": ra.get(key), "path_b": rb.get(key)}
    return None


def assert_consistent(named: dict) -> None:
    """named: {路径名: ProjectResult}。任一不一致 -> E_DIVERGE。"""
    names = sorted(named)
    base = named[names[0]]
    for other in names[1:]:
        div = first_divergence(base, named[other])
        if div is not None:
            raise diverge_error(
                f"路径 {names[0]} 与 {other} 结果不一致",
                path_a_name=names[0],
                path_b_name=other,
                **div,
            )
