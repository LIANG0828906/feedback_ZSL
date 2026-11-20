"""源骨架到目标骨架的关节映射。

匹配优先级（逐源关节尝试，命中即止）：

1. **别名表**：源关节名或其 v3 别名命中目标关节名/别名；
2. **规范化名**：:func:`retarget_pipeline.naming.normalize_name` 相等；
3. **层级路径**：规范化的根→自身路径完全一致。

未匹配关节进入报告；未被任何源关节命中的目标关节采用
“最近已映射祖先”策略回退。映射冲突（多个源关节指向同一目标）
以 :data:`~retarget_pipeline.errors.E_MAP_CONFLICT` 记录冲突对，
默认确定性地保留首个来源，其余进冲突清单（不静默丢弃）。
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field

from .errors import E_MAP_CONFLICT
from .model import AnimDocument
from .naming import hierarchy_paths, normalize_name, path_key

MATCH_ALIAS = "alias"
MATCH_NORMALIZED = "normalized_name"
MATCH_PATH = "hierarchy_path"
MATCH_ANCESTOR = "ancestor_fallback"
MATCH_NONE = "unmapped"


@dataclass
class MappingEntry:
    source_index: int
    source_name: str
    target_index: int | None
    target_name: str | None
    match_type: str
    detail: str = ""

    def to_dict(self) -> dict:
        return {
            "source_index": self.source_index,
            "source_name": self.source_name,
            "target_index": self.target_index,
            "target_name": self.target_name,
            "match_type": self.match_type,
            "detail": self.detail,
        }


@dataclass
class JointMapping:
    entries: list[MappingEntry] = field(default_factory=list)
    conflicts: list[dict] = field(default_factory=list)
    unmapped_sources: list[dict] = field(default_factory=list)
    fallback_targets: list[dict] = field(default_factory=list)

    # 以源索引为键的快速访问；target_index 为 None 表示未匹配
    def target_for_source(self, source_index: int) -> int | None:
        for entry in self.entries:
            if entry.source_index == source_index:
                return entry.target_index
        return None

    @property
    def hit_count(self) -> int:
        return sum(1 for e in self.entries if e.target_index is not None and e.match_type != MATCH_NONE)

    @property
    def source_count(self) -> int:
        return len(self.entries)

    def to_dict(self) -> dict:
        return {
            "entries": [e.to_dict() for e in self.entries],
            "conflicts": list(self.conflicts),
            "unmapped_sources": list(self.unmapped_sources),
            "fallback_targets": list(self.fallback_targets),
        }

    def canonical_json(self) -> str:
        return json.dumps(self.to_dict(), ensure_ascii=False, sort_keys=True, indent=2)

    def content_hash(self) -> str:
        return hashlib.sha256(self.canonical_json().encode("utf-8")).hexdigest()


def build_mapping(source: AnimDocument, target: AnimDocument) -> JointMapping:
    """构建确定性的源→目标关节映射（含冲突与回退报告）。"""

    # ---- 目标侧检索表 --------------------------------------------------
    target_norm: dict[str, int] = {}
    target_alias: dict[str, int] = {}
    for ti, joint in enumerate(target.joints):
        target_norm.setdefault(normalize_name(joint.name), ti)
        target_alias[normalize_name(joint.name)] = ti
        for alias in joint.aliases:
            target_alias.setdefault(normalize_name(alias), ti)

    target_paths = {path_key(p): i for i, p in
                    enumerate(hierarchy_paths(target.names, target.parents))}
    source_paths = hierarchy_paths(source.names, source.parents)

    mapping = JointMapping()
    claimed: dict[int, int] = {}  # target_index -> 首个 source_index

    for si, sjoint in enumerate(source.joints):
        source_keys = {normalize_name(sjoint.name)}
        source_keys.update(normalize_name(a) for a in sjoint.aliases)

        # 1) 别名（含目标别名表）
        hit = _first_hit(source_keys, target_alias)
        match_type = MATCH_ALIAS if hit is not None else None

        # 2) 规范化名
        if hit is None:
            hit = target_norm.get(normalize_name(sjoint.name))
            match_type = MATCH_NORMALIZED if hit is not None else None

        # 3) 层级路径
        if hit is None:
            hit = target_paths.get(path_key(source_paths[si]))
            match_type = MATCH_PATH if hit is not None else None

        if hit is None:
            entry = MappingEntry(si, sjoint.name, None, None, MATCH_NONE,
                                 detail="no alias/name/path match")
            mapping.entries.append(entry)
            mapping.unmapped_sources.append(
                {"source_index": si, "source_name": sjoint.name, "reason": MATCH_NONE}
            )
            continue

        if hit in claimed:
            first = claimed[hit]
            mapping.conflicts.append({
                "code": E_MAP_CONFLICT,
                "target_index": hit,
                "target_name": target.joints[hit].name,
                "source_index": si,
                "source_name": sjoint.name,
                "rejected_match_type": match_type,
                "kept_source_index": first,
                "kept_source_name": source.joints[first].name,
            })
            # 确定性策略：保留首个来源，后来者标记未匹配，不覆盖。
            mapping.entries.append(MappingEntry(si, sjoint.name, None, None, MATCH_NONE,
                                                detail=f"conflict target={target.joints[hit].name}"))
            mapping.unmapped_sources.append(
                {"source_index": si, "source_name": sjoint.name, "reason": E_MAP_CONFLICT,
                 "target_name": target.joints[hit].name}
            )
            continue

        claimed[hit] = si
        mapping.entries.append(MappingEntry(si, sjoint.name, hit, target.joints[hit].name,
                                            match_type or MATCH_NORMALIZED))

    _apply_ancestor_fallback(source, target, mapping, claimed)
    return mapping


def _first_hit(source_keys: set[str], target_alias: dict[str, int]) -> int | None:
    for key in sorted(k for k in source_keys if k):
        if key in target_alias:
            return target_alias[key]
    return None


def _apply_ancestor_fallback(source: AnimDocument, target: AnimDocument,
                             mapping: JointMapping, claimed: dict[int, int]) -> None:
    """未被直接命中的目标关节，回溯到最近的已映射祖先目标关节。"""

    for ti, tjoint in enumerate(target.joints):
        if ti in claimed:
            continue
        ancestor_ti = _nearest_mapped_ancestor(ti, target.parents, claimed)
        if ancestor_ti is None:
            mapping.fallback_targets.append({
                "target_index": ti, "target_name": tjoint.name,
                "resolved": False, "reason": "no mapped ancestor",
            })
            continue
        source_si = claimed[ancestor_ti]
        # 回退关系作为额外条目（source -> 额外目标），重定向时目标关节取该源驱动。
        mapping.entries.append(MappingEntry(
            source_index=source_si,
            source_name=source.joints[source_si].name,
            target_index=ti,
            target_name=tjoint.name,
            match_type=MATCH_ANCESTOR,
            detail=f"fallback via target ancestor {target.joints[ancestor_ti].name}",
        ))
        mapping.fallback_targets.append({
            "target_index": ti, "target_name": tjoint.name,
            "resolved": True, "anchor_target_index": ancestor_ti,
            "anchor_target_name": target.joints[ancestor_ti].name,
            "source_index": source_si, "source_name": source.joints[source_si].name,
        })


def _nearest_mapped_ancestor(target_index: int, parents: list[int | None],
                             claimed: dict[int, int]) -> int | None:
    cursor = parents[target_index]
    while cursor is not None and cursor >= 0:
        if cursor in claimed:
            return cursor
        cursor = parents[cursor]
    return None


def target_driver_index(mapping: JointMapping) -> dict[int, int]:
    """返回 target_index -> source_index 的驱动表（含祖先回退）。"""

    driver: dict[int, int] = {}
    for entry in mapping.entries:
        if entry.target_index is None:
            continue
        # 直接映射优先于回退；条目顺序保证直接映射先写入。
        driver.setdefault(entry.target_index, entry.source_index)
    return driver
