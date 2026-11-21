"""参数级依赖图谱与增量重算判定。

节点：源时基（``source``）、参数组件（重采样/增益/量化）、切片产物。
边：切片 → 源时基、切片 → 其所依赖的参数组件。参数版本变化后从变更
节点出发做可达性分析，仅重算受影响切片；无法证明未受影响时保守重算并
标注原因。图谱每次结构性变化递增 ``graph_version``。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum

from .parameters import TierParams
from .timebase import SourceTimebase


class NodeKind(str, Enum):
    SOURCE = "source"
    PARAM = "param"
    SLICE = "slice"


class ParamComponent(str, Enum):
    RESAMPLE = "resample"  # target_rate
    GAIN = "gain"  # gain_q8
    QUANTUM = "quantum"  # quantum_ticks / slice_span_ticks


@dataclass(frozen=True)
class GraphNode:
    node_id: str
    kind: NodeKind
    tier: str | None = None
    component: ParamComponent | None = None
    slice_index: int | None = None


@dataclass
class DependencyGraph:
    """以邻接表存储“切片/参数 → 其依赖”的有向图。"""

    source: SourceTimebase
    graph_version: int = 0
    nodes: dict[str, GraphNode] = field(default_factory=dict)
    edges: dict[str, set[str]] = field(default_factory=dict)
    # (tier, param_version, slice_index) -> 依赖的参数组件集合
    slice_deps: dict[tuple[str, int, int], set[ParamComponent]] = field(
        default_factory=dict
    )

    def _bump(self) -> None:
        self.graph_version += 1

    def ensure_source(self) -> str:
        node_id = "source:timebase"
        if node_id not in self.nodes:
            self.nodes[node_id] = GraphNode(node_id, NodeKind.SOURCE)
            self.edges.setdefault(node_id, set())
            self._bump()
        return node_id

    def ensure_param_nodes(self, tier: str) -> dict[ParamComponent, str]:
        ids = {
            comp: f"param:{tier}:{comp.value}" for comp in ParamComponent
        }
        created = False
        for comp, node_id in ids.items():
            if node_id not in self.nodes:
                self.nodes[node_id] = GraphNode(
                    node_id, NodeKind.PARAM, tier=tier, component=comp
                )
                self.edges.setdefault(node_id, set())
                created = True
        if created:
            self._bump()
        return ids

    def register_slice(
        self,
        tier: str,
        param_version: int,
        slice_index: int,
        params: TierParams,
    ) -> str:
        """登记切片节点及其对源时基、三个参数组件的依赖边。"""

        self.ensure_source()
        param_ids = self.ensure_param_nodes(tier)
        node_id = f"slice:{tier}:v{param_version}:{slice_index}"
        components = set(ParamComponent)
        if node_id not in self.nodes:
            self.nodes[node_id] = GraphNode(
                node_id,
                NodeKind.SLICE,
                tier=tier,
                slice_index=slice_index,
            )
            self._bump()
        deps = {"source:timebase", *param_ids.values()}
        if self.edges.get(node_id) != deps:
            self.edges[node_id] = set(deps)
            self._bump()
        self.slice_deps[(tier, param_version, slice_index)] = components
        return node_id

    def changed_components(
        self, old: TierParams | None, new: TierParams
    ) -> set[ParamComponent]:
        """比较两版参数，返回发生变化的参数组件。"""
        if old is None:
            return set(ParamComponent)
        changed: set[ParamComponent] = set()
        if old.target_rate != new.target_rate:
            changed.add(ParamComponent.RESAMPLE)
        if old.gain_q8 != new.gain_q8:
            changed.add(ParamComponent.GAIN)
        if (
            old.quantum_ticks != new.quantum_ticks
            or old.slice_span_ticks != new.slice_span_ticks
        ):
            changed.add(ParamComponent.QUANTUM)
        return changed

    def plan_recompute(
        self,
        tier: str,
        old_params: TierParams | None,
        new_params: TierParams,
        old_slice_count: int,
        new_slice_count: int,
        proven_clean: set[int] | None = None,
    ) -> "RecomputePlan":
        """计算受影响切片集合与复用/重算清单。

        - 量化组件变化或切片数量变化：网格整体改变，无法证明任何切片
          边界不变，保守重算全部并标注原因；
        - 仅重采样/增益变化：只影响音频缓冲，逐片比对依赖组件，未触及
          的切片允许复用；
        - ``proven_clean`` 之外且无法证明未受影响的切片保守重算。
        """

        changed = self.changed_components(old_params, new_params)
        recompute: set[int] = set()
        reuse: set[int] = set()
        reasons: dict[int, str] = {}

        grid_changed = ParamComponent.QUANTUM in changed

        if old_params is None:
            for idx in range(new_slice_count):
                recompute.add(idx)
                reasons[idx] = "首版参数，无缓存可复用"
        elif grid_changed:
            for idx in range(new_slice_count):
                recompute.add(idx)
                reasons[idx] = "量化刻度/切片跨度变化，边界无法证明不变"
        else:
            for idx in range(new_slice_count):
                if idx >= old_slice_count:
                    recompute.add(idx)
                    reasons[idx] = "新增切片，历史缓存不存在"
                    continue
                # 所有切片都依赖三个参数组件；从变更节点可达即受影响。
                if changed:
                    recompute.add(idx)
                    reasons[idx] = (
                        "参数组件变化: "
                        + ",".join(sorted(c.value for c in changed))
                    )
                elif proven_clean is not None and idx not in proven_clean:
                    recompute.add(idx)
                    reasons[idx] = "无法证明切片未受影响，保守重算"
                else:
                    reuse.add(idx)

        return RecomputePlan(
            tier=tier,
            changed_components=changed,
            recompute=sorted(recompute),
            reuse=sorted(i for i in reuse if i < new_slice_count),
            reasons=reasons,
            new_slice_count=new_slice_count,
            graph_version=self.graph_version,
        )


@dataclass(frozen=True)
class RecomputePlan:
    """一次参数变化后的增量重算计划。"""

    tier: str
    changed_components: set[ParamComponent]
    recompute: list[int]
    reuse: list[int]
    reasons: dict[int, str]
    new_slice_count: int
    graph_version: int

    def to_dict(self) -> dict:
        return {
            "tier": self.tier,
            "changed_components": sorted(c.value for c in self.changed_components),
            "recompute_slices": self.recompute,
            "reuse_slices": self.reuse,
            "recompute_count": len(self.recompute),
            "reuse_count": len(self.reuse),
            "reasons": {str(k): v for k, v in sorted(self.reasons.items())},
            "new_slice_count": self.new_slice_count,
            "graph_version": self.graph_version,
        }
