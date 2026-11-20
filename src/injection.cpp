#include "battle/injection.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <memory>
#include <sstream>

namespace battle {
namespace {

Error injected_error(StatusCode code, std::string message, std::string point) {
    Error error;
    error.code = code;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

Outcome<void> ensure_snapshot(const RegionStore& store,
                              const SnapshotHandle& handle) {
    Outcome<PublishedRegionPtr> snapshot = validate_query_snapshot(
        store, handle, 0, {});
    if (!snapshot) {
        return Outcome<void>::failure(snapshot.error());
    }
    return Outcome<void>::success();
}

Outcome<PickResult> corrupt_snapshot_pick(const RegionStore& store,
                                          const SnapshotHandle& handle,
                                          const PickRequest& request) {
    Outcome<PublishedRegionPtr> resolved = validate_query_snapshot(
        store, handle, 0, {});
    if (!resolved) {
        return Outcome<PickResult>::failure(resolved.error());
    }
    auto corrupted = std::make_shared<PublishedRegion>(*resolved.value());
    if (!corrupted->state.units.empty()) {
        Unit& unit = corrupted->state.units.front();
        if (unit.shape == UnitShape::Aabb) {
            unit.aabb.min.x += 0.125F;
        } else {
            unit.capsule.radius *= 1.125F;
        }
    } else if (!corrupted->state.grid.walkable.empty()) {
        corrupted->state.grid.walkable[0] ^= 1U;
    } else {
        corrupted->state.version += 1;
    }
    Outcome<PickResult> result =
        pick_screen_snapshot(corrupted, request);
    if (!result) {
        result.error().injection_point = "injection.corrupt_snapshot";
    }
    return result;
}

Outcome<PickResult> degenerate_bounds_pick(const RegionStore& store,
                                           const SnapshotHandle& handle,
                                           const PickRequest& request,
                                           const InjectionSpec& injection) {
    (void)request;
    Outcome<PublishedRegionPtr> resolved = validate_query_snapshot(
        store, handle, 0, {});
    if (!resolved) {
        return Outcome<PickResult>::failure(resolved.error());
    }
    RegionState state = resolved.value()->state;
    std::uint32_t target = injection.target_unit;
    if (target == 0 && !state.units.empty()) {
        target = state.units.front().id;
    }
    const auto it = std::lower_bound(
        state.units.begin(), state.units.end(), target,
        [](const Unit& unit, std::uint32_t id) { return unit.id < id; });
    if (it == state.units.end() || it->id != target) {
        Error error = injected_error(
            StatusCode::SceneInvalid,
            "injected degenerate bounds target unit does not exist",
            "injection.degenerate_bounds.missing_entity");
        error.entity = {EntityKind::Unit, target};
        return Outcome<PickResult>::failure(std::move(error));
    }
    if (it->shape == UnitShape::Aabb) {
        const glm::vec3 center = (it->aabb.min + it->aabb.max) * 0.5F;
        it->aabb.min = center;
        it->aabb.max = center;
    } else {
        it->capsule.radius = 0.0F;
    }
    Scene scene;
    scene.grid = state.grid;
    scene.units = state.units;
    scene.covers = state.covers;
    Outcome<void> valid = scene.validate();
    if (!valid) {
        Error error = valid.error();
        error.injection_point = "injection.degenerate_bounds";
        error.entity = {EntityKind::Unit, target};
        return Outcome<PickResult>::failure(std::move(error));
    }
    return Outcome<PickResult>::failure(injected_error(
        StatusCode::SceneInvalid,
        "injected degenerate bounds were not rejected by scene validation",
        "injection.degenerate_bounds"));
}

}  // namespace

const char* injection_kind_name(InjectionKind kind) noexcept {
    switch (kind) {
        case InjectionKind::None: return "NONE";
        case InjectionKind::DegenerateUnitBounds: return "DEGENERATE_UNIT_BOUNDS";
        case InjectionKind::ZeroLengthRay: return "ZERO_LENGTH_RAY";
        case InjectionKind::OutOfBoundsScreen: return "OUT_OF_BOUNDS_SCREEN";
        case InjectionKind::SelfIntersectingDraft:
            return "SELF_INTERSECTING_DRAFT";
        case InjectionKind::CorruptSnapshot: return "CORRUPT_SNAPSHOT";
    }
    return "UNKNOWN";
}

Outcome<PickResult> InjectionGate::pick_screen(
    const RegionStore& store, const SnapshotHandle& handle,
    const PickRequest& request, const InjectionSpec& injection) {
    switch (injection.kind) {
        case InjectionKind::None:
            return battle::pick_screen(store, handle, request);
        case InjectionKind::DegenerateUnitBounds:
            return degenerate_bounds_pick(store, handle, request, injection);
        case InjectionKind::OutOfBoundsScreen: {
            Outcome<void> snapshot = ensure_snapshot(store, handle);
            if (!snapshot) {
                return Outcome<PickResult>::failure(snapshot.error());
            }
            if (!is_finite(request.screen.x) || !is_finite(request.screen.y) ||
                request.screen.x < 0.0F || request.screen.y < 0.0F ||
                request.screen.x > request.camera.viewport_width ||
                request.screen.y > request.camera.viewport_height) {
                return Outcome<PickResult>::failure(injected_error(
                    StatusCode::RayInvalid,
                    "injected screen coordinate is outside the viewport",
                    "injection.out_of_bounds_screen"));
            }
            return Outcome<PickResult>::failure(injected_error(
                StatusCode::RayInvalid,
                "injected out-of-bounds screen value was unexpectedly valid",
                "injection.out_of_bounds_screen"));
        }
        case InjectionKind::CorruptSnapshot:
            return corrupt_snapshot_pick(store, handle, request);
        case InjectionKind::ZeroLengthRay:
        case InjectionKind::SelfIntersectingDraft:
            return Outcome<PickResult>::failure(injected_error(
                StatusCode::UsageError,
                "injection kind is not valid for screen picking",
                "injection.channel"));
    }
    return Outcome<PickResult>::failure(injected_error(
        StatusCode::UsageError, "unknown injection kind", "injection.channel"));
}

Outcome<PickResult> InjectionGate::pick_ray(
    const RegionStore& store, const SnapshotHandle& handle, Ray ray,
    const InjectionSpec& injection) {
    if (injection.kind == InjectionKind::None) {
        return battle::pick_ray(store, handle, ray);
    }
    Outcome<void> snapshot = ensure_snapshot(store, handle);
    if (!snapshot) {
        return Outcome<PickResult>::failure(snapshot.error());
    }
    if (injection.kind == InjectionKind::ZeroLengthRay) {
        ray.direction = glm::vec3(0.0F);
        Outcome<Ray> validated = validate_ray(ray, "injection.zero_length_ray");
        if (!validated) {
            return Outcome<PickResult>::failure(validated.error());
        }
        return Outcome<PickResult>::failure(injected_error(
            StatusCode::RayInvalid,
            "injected zero-length ray was unexpectedly accepted",
            "injection.zero_length_ray"));
    }
    if (injection.kind == InjectionKind::CorruptSnapshot) {
        PickRequest request;
        request.camera.position = ray.origin;
        request.camera.forward = ray.direction;
        request.camera.up = glm::vec3(0.0F, 1.0F, 0.0F);
        if (glm::length(glm::cross(request.camera.forward,
                                   request.camera.up)) < 1.0e-6F) {
            request.camera.up = glm::vec3(1.0F, 0.0F, 0.0F);
        }
        request.screen = {request.camera.viewport_width * 0.5F,
                          request.camera.viewport_height * 0.5F};
        return corrupt_snapshot_pick(store, handle, request);
    }
    return Outcome<PickResult>::failure(injected_error(
        StatusCode::UsageError,
        "injection kind is not valid for ray picking",
        "injection.channel"));
}

Outcome<PlacementResult> InjectionGate::placement(
    const RegionStore& store, const SnapshotHandle& handle,
    const PlacementRequest& request, const InjectionSpec& injection) {
    if (injection.kind == InjectionKind::None) {
        return battle::validate_placement(store, handle, request);
    }
    if (injection.kind != InjectionKind::SelfIntersectingDraft) {
        return Outcome<PlacementResult>::failure(injected_error(
            StatusCode::UsageError,
            "injection kind is not valid for draft placement",
            "injection.channel"));
    }
    Outcome<PublishedRegionPtr> resolved = validate_query_snapshot(
        store, handle, request.expected_version, request.expected_hash);
    if (!resolved) {
        return Outcome<PlacementResult>::failure(resolved.error());
    }
    const Grid& grid = resolved.value()->state.grid;
    const float base_y = request.region.vertices.empty()
                             ? grid.ground_y()
                             : request.region.vertices.front().y;
    glm::vec2 center(grid.origin.x + static_cast<float>(grid.width) *
                                          grid.cell_size * 0.5F,
                     grid.origin.z + static_cast<float>(grid.depth) *
                                          grid.cell_size * 0.5F);
    if (!request.region.vertices.empty()) {
        center = {request.region.vertices.front().x,
                  request.region.vertices.front().z};
    }
    const float half = grid.cell_size * 0.75F;
    PlacementRequest mutated = request;
    mutated.region.vertices = {
        {center.x - half, base_y, center.y - half},
        {center.x + half, base_y, center.y + half},
        {center.x + half, base_y, center.y - half},
        {center.x - half, base_y, center.y + half}};
    Outcome<PlacementResult> result =
        validate_placement(store, handle, mutated);
    if (!result) {
        result.error().injection_point =
            "injection.self_intersecting_draft";
        return result;
    }
    return Outcome<PlacementResult>::failure(injected_error(
        StatusCode::RegionInvalid,
        "injected self-intersecting draft was unexpectedly accepted",
        "injection.self_intersecting_draft"));
}

}  // namespace battle
