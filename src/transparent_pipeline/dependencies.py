"""Explicit per-block resource dependencies and invalidation propagation."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Iterable

MEDIA_FIELDS = ("boundary", "ior", "absorption", "scattering")
LIGHT_FIELDS = ("direction", "radiance", "anisotropy")


@dataclass
class DependencyGraph:
    block_media: dict[int, set[str]] = field(default_factory=dict)
    block_lights: dict[int, set[str]] = field(default_factory=dict)
    block_media_fields: dict[int, dict[str, set[str]]] = field(default_factory=dict)
    block_light_fields: dict[int, dict[str, set[str]]] = field(default_factory=dict)

    @classmethod
    def from_render(
        cls,
        media_dependencies: dict[int, Iterable[str]],
        light_dependencies: dict[int, Iterable[str]],
    ) -> "DependencyGraph":
        block_ids = sorted(set(media_dependencies) | set(light_dependencies))
        graph = cls()
        for block_id in block_ids:
            graph.set_block(
                block_id,
                media_dependencies.get(block_id, ()),
                light_dependencies.get(block_id, ()),
            )
        return graph

    def set_block(self, block_id: int, media: Iterable[str], lights: Iterable[str]) -> None:
        media_ids = set(media)
        light_ids = set(lights)
        self.block_media[block_id] = media_ids
        self.block_lights[block_id] = light_ids
        self.block_media_fields[block_id] = {media_id: set(MEDIA_FIELDS) for media_id in media_ids}
        self.block_light_fields[block_id] = {light_id: set(LIGHT_FIELDS) for light_id in light_ids}

    def remove_block(self, block_id: int) -> None:
        self.block_media.pop(block_id, None)
        self.block_lights.pop(block_id, None)
        self.block_media_fields.pop(block_id, None)
        self.block_light_fields.pop(block_id, None)

    def blocks_for_media(self, media_id: str) -> list[int]:
        return sorted(block_id for block_id, resources in self.block_media.items() if media_id in resources)

    def blocks_for_light(self, light_id: str) -> list[int]:
        return sorted(block_id for block_id, resources in self.block_lights.items() if light_id in resources)

    def downstream(self, block_id: int, *, include_media: bool = True, include_lights: bool = False) -> list[int]:
        result: set[int] = set()
        for later in sorted(self.block_media):
            if later <= block_id:
                continue
            if include_media and self.block_media.get(later, set()) & self.block_media.get(block_id, set()):
                result.add(later)
            if include_lights and self.block_lights.get(later, set()) & self.block_lights.get(block_id, set()):
                result.add(later)
        return sorted(result)

    def invalidation_manifest(
        self,
        *,
        resource_id: str,
        resource_kind: str,
        field_names: list[str],
        stage: str,
        direct_blocks: list[int],
        downstream_blocks: list[int],
        extra_blocks: list[int] | None = None,
        extra_reason: str = "geometry_projection_overlap",
    ) -> list[dict[str, Any]]:
        direct = set(direct_blocks)
        downstream = set(downstream_blocks)
        extra = set(extra_blocks or [])
        entries: list[dict[str, Any]] = []
        for block_id in sorted(direct | downstream | extra):
            if block_id in direct:
                reason = "direct_dependency"
            elif block_id in downstream:
                reason = "downstream_dependency"
            else:
                reason = extra_reason
            entries.append(
                {
                    "block_id": block_id,
                    "resource_kind": resource_kind,
                    "resource_id": resource_id,
                    "field": ",".join(field_names),
                    "stage": stage,
                    "reason": reason,
                }
            )
        return entries

    def to_dict(self) -> dict[str, Any]:
        return {
            "block_media": {str(block): sorted(values) for block, values in sorted(self.block_media.items())},
            "block_lights": {str(block): sorted(values) for block, values in sorted(self.block_lights.items())},
            "block_media_fields": {
                str(block): {media_id: sorted(fields) for media_id, fields in sorted(values.items())}
                for block, values in sorted(self.block_media_fields.items())
            },
            "block_light_fields": {
                str(block): {light_id: sorted(fields) for light_id, fields in sorted(values.items())}
                for block, values in sorted(self.block_light_fields.items())
            },
            "downstream": {
                str(block): self.downstream(block, include_lights=True)
                for block in sorted(self.block_media)
            },
        }


__all__ = ["DependencyGraph", "LIGHT_FIELDS", "MEDIA_FIELDS"]
