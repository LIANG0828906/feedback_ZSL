"""关节名规范化与层级路径工具。

规范化规则（确定性、与大小写/分隔符风格无关）：

1. 转小写；
2. 把 ``-``、空格、点统一为下划线；
3. 剔除除字母数字下划线外的字符；
4. 折叠连续下划线并去除首尾下划线。

例如 ``"L_Hand "``、``"l-hand"``、``"L.Hand"`` 均规范化为 ``"l_hand"``。
"""

from __future__ import annotations

import re

_SUB_RE = re.compile(r"[^a-z0-9_]+")
_COLLAPSE_RE = re.compile(r"_+")


def normalize_name(name: str) -> str:
    if name is None:
        return ""
    text = str(name).strip().lower().replace("-", "_").replace(" ", "_").replace(".", "_")
    text = _SUB_RE.sub("_", text)
    text = _COLLAPSE_RE.sub("_", text).strip("_")
    return text


def hierarchy_paths(names: list[str], parents: list[int | None]) -> list[list[str]]:
    """返回每个关节从根到自身的规范化名称路径。"""

    paths: list[list[str] | None] = [None] * len(names)

    def build(idx: int) -> list[str]:
        if paths[idx] is not None:
            return paths[idx]  # type: ignore[return-value]
        own = normalize_name(names[idx])
        parent = parents[idx]
        if parent is None or parent < 0:
            paths[idx] = [own]
        else:
            paths[idx] = build(int(parent)) + [own]
        return paths[idx]  # type: ignore[return-value]

    for i in range(len(names)):
        build(i)
    return [p or [] for p in paths]


def path_key(path: list[str]) -> str:
    return "/".join(path)
