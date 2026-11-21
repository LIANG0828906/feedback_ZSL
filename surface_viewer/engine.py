from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Mapping

from .artifacts import (
    ARTIFACT_ORDER,
    Artifact,
    compare_artifact_bytes,
    encode_density,
    encode_mesh,
    encode_normals,
)
from .errors import ErrorCode, SurfaceViewerError
from .geometry import AssemblyTopology, SurfaceGeometry
from .graph import (
    DependencyGraph,
    GraphDiff,
    InvalidationReport,
    InvalidatedArtifact,
    artifact_node_id,
    changed_parameter_node_ids,
    structurally_changed_patches,
)
from .model import SurfaceAssembly, SurfacePatch
from .preview import (
    OpenGLOffscreenPreviewRenderer,
    PreviewRenderer,
    ScreenPreview,
    SoftwarePreviewRenderer,
    compose_screen_preview,
    preview_layout,
)


@dataclass(frozen=True)
class ResourceLimits:
    max_patches: int = 10_000
    max_vertices_per_patch: int = 250_000
    max_artifacts: int = 40_000
    max_preview_bytes: int = 256 * 1024 * 1024


@dataclass(frozen=True)
class UpdateResult:
    previous_assembly: SurfaceAssembly | None
    assembly: SurfaceAssembly
    previous_graph: DependencyGraph | None
    graph: DependencyGraph
    graph_diff: GraphDiff
    invalidations: InvalidationReport
    artifacts: tuple[Artifact, ...]
    screen_preview: ScreenPreview
    recomputed_ids: tuple[str, ...]
    reused_ids: tuple[str, ...]
    verification: Mapping[str, object] | None = None

    @property
    def artifact_map(self) -> dict[str, Artifact]:
        return {artifact.artifact_id: artifact for artifact in self.artifacts}

    @property
    def content_hashes(self) -> dict[str, str]:
        return {
            artifact.artifact_id: artifact.content_hash
            for artifact in sorted(self.artifacts, key=lambda item: item.artifact_id)
        }

    @property
    def affected_patches(self) -> tuple[str, ...]:
        return self.invalidations.affected_patches

    def to_trace_fields(self) -> dict[str, object]:
        return {
            "assembly_version": self.assembly.version_hash,
            "graph": {
                "node_count": self.graph.node_count,
                "edge_count": self.graph.edge_count,
                "diff": self.graph_diff.to_dict(),
            },
            "changed_parameters": list(self.invalidations.changed_parameters),
            "affected_artifacts": list(self.invalidations.affected_ids),
            "affected_patches": list(self.affected_patches),
            "dependent_closure": list(self.invalidations.dependency_closure),
            "invalidations": [
                item.to_dict() for item in self.invalidations.affected
            ],
            "reused_artifacts": list(self.reused_ids),
            "recomputed_artifacts": list(self.recomputed_ids),
            "reused_count": len(self.reused_ids),
            "recomputed_count": len(self.recomputed_ids),
            "conservative_recomputes": [
                item.to_dict() for item in self.invalidations.conservative
            ],
            "content_hashes": self.content_hashes,
            "screen_preview_hash": self.screen_preview.content_hash,
            "dirty_rects": [
                list(rectangle) for rectangle in self.screen_preview.dirty_rects
            ],
            "verification": dict(self.verification or {}),
        }


class IncrementalSurfaceEngine:
    def __init__(
        self,
        assembly: SurfaceAssembly,
        *,
        preview_backend: str | PreviewRenderer = "software",
        resource_limits: ResourceLimits | None = None,
    ):
        self.limits = resource_limits or ResourceLimits()
        self._validate_resources(assembly)
        self.assembly = assembly
        self.topology = AssemblyTopology(assembly)
        self.geometry = SurfaceGeometry(self.topology)
        self.preview_renderer = _make_preview_renderer(preview_backend)
        self.graph = DependencyGraph.build(assembly)
        self.graph.assert_publishable()
        self.cache: dict[str, Artifact] = {}
        self.screen_preview: ScreenPreview | None = None

    def _validate_resources(self, assembly: SurfaceAssembly) -> None:
        if len(assembly.patches) > self.limits.max_patches:
            raise SurfaceViewerError(
                ErrorCode.RESOURCE_LIMIT,
                "assembly exceeds the patch resource limit",
                details={
                    "patch_count": len(assembly.patches),
                    "maximum": self.limits.max_patches,
                },
            )
        artifact_count = len(assembly.patches) * 4
        if artifact_count > self.limits.max_artifacts:
            raise SurfaceViewerError(
                ErrorCode.RESOURCE_LIMIT,
                "assembly exceeds the artifact resource limit",
                details={
                    "artifact_count": artifact_count,
                    "maximum": self.limits.max_artifacts,
                },
            )
        for patch in assembly.patches:
            vertex_count = (patch.segments + 1) ** 2
            if vertex_count > self.limits.max_vertices_per_patch:
                raise SurfaceViewerError(
                    ErrorCode.RESOURCE_LIMIT,
                    "patch exceeds the vertex resource limit",
                    field=f"patches[{patch.patch_id}].precision_level",
                    details={
                        "patch_id": patch.patch_id,
                        "vertex_count": vertex_count,
                        "maximum": self.limits.max_vertices_per_patch,
                    },
                )
        columns, rows, tile_size = preview_layout(assembly)
        preview_bytes = columns * rows * tile_size * tile_size * 4
        if preview_bytes > self.limits.max_preview_bytes:
            raise SurfaceViewerError(
                ErrorCode.RESOURCE_LIMIT,
                "assembly exceeds the offscreen preview resource limit",
                field="screen_preview",
                details={
                    "preview_bytes": preview_bytes,
                    "maximum": self.limits.max_preview_bytes,
                },
            )

    def initialize(self, *, verify_full: bool = True) -> UpdateResult:
        all_parameter_ids = tuple(
            sorted(
                node.node_id
                for node in self.graph.nodes.values()
                if node.node_type == "parameter"
            )
        )
        report = self.graph.invalidate(all_parameter_ids)
        artifacts = self._ensure_artifacts(
            self.assembly,
            self.graph,
            previous_cache={},
            force_ids={node.node_id for node in self.graph.artifact_nodes()},
        )
        screen = compose_screen_preview(self.assembly, artifacts.values())
        verification = (
            self._verify_full(self.assembly, artifacts, screen)
            if verify_full
            else None
        )
        self.cache = artifacts
        self.screen_preview = screen
        result = UpdateResult(
            previous_assembly=None,
            assembly=self.assembly,
            previous_graph=None,
            graph=self.graph,
            graph_diff=self.graph.diff(None),
            invalidations=report,
            artifacts=tuple(
                sorted(artifacts.values(), key=lambda artifact: artifact.artifact_id)
            ),
            screen_preview=screen,
            recomputed_ids=tuple(sorted(artifacts)),
            reused_ids=(),
            verification=verification,
        )
        return result

    def update(
        self,
        patch_id: str,
        changes: Mapping[str, object],
        *,
        verify_full: bool = True,
    ) -> UpdateResult:
        previous_assembly = self.assembly
        previous_graph = self.graph
        new_assembly = previous_assembly.apply_changes(patch_id, changes)
        self._validate_resources(new_assembly)
        new_graph = DependencyGraph.build(new_assembly)
        new_graph.assert_publishable()
        new_topology = AssemblyTopology(new_assembly)
        new_geometry = SurfaceGeometry(new_topology)

        changed_ids = changed_parameter_node_ids(previous_assembly, new_assembly)
        structural = structurally_changed_patches(
            previous_assembly, new_assembly
        )
        cached_signatures = {
            artifact_id: artifact.parameter_signature
            for artifact_id, artifact in self.cache.items()
        }
        report = new_graph.invalidate(
            changed_ids,
            cached_signatures=cached_signatures,
            structural_changed_patches=structural,
        )
        force_ids = set(report.affected_ids)
        artifacts = self._ensure_artifacts(
            new_assembly,
            new_graph,
            previous_cache=self.cache,
            force_ids=force_ids,
            geometry=new_geometry,
        )

        dirty_patch_ids = {
            item.patch_id
            for item in report.affected
            if item.kind == "preview"
        }
        if self.screen_preview is None:
            screen = compose_screen_preview(new_assembly, artifacts.values())
        else:
            screen = compose_screen_preview(
                new_assembly,
                artifacts.values(),
                previous=self.screen_preview,
                dirty_patch_ids=dirty_patch_ids,
            )

        verification = (
            self._verify_full(new_assembly, artifacts, screen)
            if verify_full
            else None
        )

        recomputed = tuple(
            sorted(
                artifact_id
                for artifact_id, artifact in artifacts.items()
                if artifact_id not in self.cache
                or artifact.content_hash != self.cache[artifact_id].content_hash
                or artifact_id in force_ids
            )
        )
        reused = tuple(sorted(set(artifacts) - set(recomputed)))

        self.assembly = new_assembly
        self.graph = new_graph
        self.topology = new_topology
        self.geometry = new_geometry
        self.cache = artifacts
        self.screen_preview = screen
        return UpdateResult(
            previous_assembly=previous_assembly,
            assembly=new_assembly,
            previous_graph=previous_graph,
            graph=new_graph,
            graph_diff=new_graph.diff(previous_graph),
            invalidations=report,
            artifacts=tuple(
                sorted(artifacts.values(), key=lambda artifact: artifact.artifact_id)
            ),
            screen_preview=screen,
            recomputed_ids=recomputed,
            reused_ids=reused,
            verification=verification,
        )

    @classmethod
    def recover(
        cls,
        assembly: SurfaceAssembly,
        committed_artifacts: Mapping[str, Artifact],
        *,
        preview_backend: str | PreviewRenderer = "software",
        resource_limits: ResourceLimits | None = None,
        verify_full: bool = True,
    ) -> tuple["IncrementalSurfaceEngine", UpdateResult]:
        engine = cls(
            assembly,
            preview_backend=preview_backend,
            resource_limits=resource_limits,
        )
        expected_ids = {
            node.node_id for node in engine.graph.artifact_nodes()
        }
        directly_reusable: dict[str, Artifact] = {}
        stale_ids: set[str] = set()
        for artifact_id in sorted(expected_ids):
            candidate = committed_artifacts.get(artifact_id)
            expected_signature = engine.graph.artifact_signature(artifact_id)
            if (
                candidate is not None
                and candidate.parameter_signature == expected_signature
            ):
                directly_reusable[artifact_id] = candidate
            else:
                stale_ids.add(artifact_id)

        artifacts = engine._ensure_artifacts(
            assembly,
            engine.graph,
            previous_cache=directly_reusable,
            force_ids=stale_ids,
        )
        screen = compose_screen_preview(assembly, artifacts.values())
        verification = (
            engine._verify_full(assembly, artifacts, screen)
            if verify_full
            else None
        )
        reused = tuple(sorted(set(artifacts) - stale_ids))
        affected = tuple(
            InvalidatedArtifact(
                artifact_id=artifact_id,
                patch_id=engine.graph.nodes[artifact_id].patch_id,
                kind=engine.graph.nodes[artifact_id].name,
                reasons=("committed artifact is missing or stale",),
            )
            for artifact_id in sorted(stale_ids)
        )
        report = InvalidationReport(
            changed_parameters=(),
            dependency_closure=tuple(sorted(stale_ids)),
            affected=affected,
            reused=reused,
            conservative=(),
        )
        engine.cache = artifacts
        engine.screen_preview = screen
        result = UpdateResult(
            previous_assembly=None,
            assembly=assembly,
            previous_graph=None,
            graph=engine.graph,
            graph_diff=engine.graph.diff(None),
            invalidations=report,
            artifacts=tuple(
                sorted(artifacts.values(), key=lambda artifact: artifact.artifact_id)
            ),
            screen_preview=screen,
            recomputed_ids=tuple(sorted(stale_ids)),
            reused_ids=reused,
            verification=verification,
        )
        return engine, result

    def _ensure_artifacts(
        self,
        assembly: SurfaceAssembly,
        graph: DependencyGraph,
        previous_cache: Mapping[str, Artifact],
        force_ids: Iterable[str],
        *,
        geometry: SurfaceGeometry | None = None,
    ) -> dict[str, Artifact]:
        force = set(force_ids)
        active_geometry = geometry or self.geometry
        artifacts: dict[str, Artifact] = {}
        for patch in sorted(assembly.patches, key=lambda item: item.patch_id):
            for kind in sorted(
                ("mesh", "normals", "density", "preview"),
                key=lambda value: ARTIFACT_ORDER[value],
            ):
                artifact_id = artifact_node_id(patch.patch_id, kind)
                expected_signature = parameter_signature_for_kind(patch, kind)
                cached = previous_cache.get(artifact_id)
                if (
                    artifact_id not in force
                    and cached is not None
                    and cached.parameter_signature == expected_signature
                ):
                    artifacts[artifact_id] = cached
                    continue
                artifact = self._compute_artifact(
                    patch,
                    kind,
                    expected_signature,
                    artifacts,
                    active_geometry,
                )
                artifacts[artifact_id] = artifact
        return artifacts

    def _compute_artifact(
        self,
        patch: SurfacePatch,
        kind: str,
        signature: str,
        current: Mapping[str, Artifact],
        geometry: SurfaceGeometry,
    ) -> Artifact:
        if kind == "mesh":
            data = encode_mesh(geometry.compute_mesh(patch))
        elif kind == "normals":
            data = encode_normals(geometry.compute_normals(patch))
        elif kind == "density":
            data = encode_density(patch.segments, patch.segments)
        elif kind == "preview":
            mesh_id = artifact_node_id(patch.patch_id, "mesh")
            normals_id = artifact_node_id(patch.patch_id, "normals")
            mesh_artifact = current.get(mesh_id)
            normals_artifact = current.get(normals_id)
            if mesh_artifact is None or normals_artifact is None:
                raise SurfaceViewerError(
                    ErrorCode.DEP_GRAPH_INCOMPLETE,
                    "preview artifact cannot be computed before its mesh and normals",
                    field=artifact_node_id(patch.patch_id, "preview"),
                )
            data = self.preview_renderer.render_tile(
                patch, mesh_artifact, normals_artifact
            )
        else:
            raise ValueError(kind)
        return Artifact.create(
            patch_id=patch.patch_id,
            kind=kind,
            data=data,
            parameter_signature=signature,
        )

    def _verify_full(
        self,
        assembly: SurfaceAssembly,
        incremental_artifacts: Mapping[str, Artifact],
        incremental_screen: ScreenPreview,
    ) -> dict[str, object]:
        full_engine = IncrementalSurfaceEngine(
            assembly,
            preview_backend=self.preview_renderer,
            resource_limits=self.limits,
        )
        full_artifacts = full_engine._ensure_artifacts(
            assembly,
            full_engine.graph,
            previous_cache={},
            force_ids={
                node.node_id for node in full_engine.graph.artifact_nodes()
            },
        )
        for artifact_id, expected in sorted(full_artifacts.items()):
            actual = incremental_artifacts.get(artifact_id)
            if actual is None:
                raise SurfaceViewerError(
                    ErrorCode.INCR_MISMATCH,
                    "incremental result is missing an artifact",
                    field=artifact_id,
                    details={"artifact_id": artifact_id},
                )
            difference = compare_artifact_bytes(expected, actual)
            if difference is not None:
                raise SurfaceViewerError(
                    ErrorCode.INCR_MISMATCH,
                    "incremental product differs from full recomputation",
                    field=artifact_id,
                    details=difference,
                )

        full_screen = compose_screen_preview(assembly, full_artifacts.values())
        if full_screen.rgba != incremental_screen.rgba:
            limit = min(len(full_screen.rgba), len(incremental_screen.rgba))
            first = next(
                (
                    index
                    for index in range(limit)
                    if full_screen.rgba[index] != incremental_screen.rgba[index]
                ),
                limit,
            )
            raise SurfaceViewerError(
                ErrorCode.INCR_MISMATCH,
                "incremental offscreen preview differs from full rerender",
                field="screen_preview",
                details={
                    "first_difference_byte": first,
                    "expected_hash": full_screen.content_hash,
                    "actual_hash": incremental_screen.content_hash,
                },
            )
        return {
            "byte_exact": True,
            "artifacts_checked": len(full_artifacts),
            "full_recompute_used_as_truth": True,
            "screen_preview_hash": full_screen.content_hash,
            "screen_preview_bytes": len(full_screen.rgba),
        }


def _make_preview_renderer(
    backend: str | PreviewRenderer,
) -> PreviewRenderer:
    if not isinstance(backend, str):
        return backend
    if backend == "software":
        return SoftwarePreviewRenderer()
    if backend == "opengl":
        renderer = OpenGLOffscreenPreviewRenderer()
        if not renderer.available():
            raise SurfaceViewerError(
                ErrorCode.RESOURCE_LIMIT,
                "OpenGL offscreen backend is unavailable in this environment",
            )
        return renderer
    raise ValueError(f"unknown preview backend: {backend}")


def parameter_signature_for_kind(patch: SurfacePatch, kind: str) -> str:
    from .graph import parameter_signature_for_patch

    return parameter_signature_for_patch(patch, kind)
