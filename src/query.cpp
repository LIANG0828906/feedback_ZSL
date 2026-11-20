#include "battle/query.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <cmath>
#include <limits>

namespace battle {
namespace {

constexpr float kIntersectEpsilon = 1.0e-7F;

struct Candidate {
    bool found = false;
    EntityRef entity;
    float t = 0.0F;
    glm::vec3 point{0.0F};
    glm::vec3 normal{0.0F, 1.0F, 0.0F};
    int priority = 100;
};

Error query_error(StatusCode code, std::string message, std::string point) {
    Error error;
    error.code = code;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    return error;
}

Outcome<void> verify_snapshot_integrity(const PublishedRegionPtr& snapshot,
                                        const SnapshotHandle& handle) {
    if (!snapshot) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale, "resolved snapshot is empty",
            "snapshot.integrity"));
    }
    if (snapshot->state.version != handle.version) {
        return Outcome<void>::failure(query_error(
            StatusCode::RegionStale, "handle and snapshot versions differ",
            "region.version"));
    }
    if (snapshot->state.content_hash != handle.content_hash) {
        return Outcome<void>::failure(query_error(
            StatusCode::RegionStale, "handle and snapshot hashes differ",
            "region.hash"));
    }
    if (snapshot->state.compute_hash() != snapshot->state.content_hash) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale, "snapshot canonical hash mismatch",
            "snapshot.integrity.hash"));
    }
    if (snapshot->unit_bvh.primitive_bounds().size() != snapshot->state.units.size() ||
        snapshot->cover_bvh.primitive_bounds().size() != snapshot->state.covers.size()) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale,
            "snapshot acceleration structure does not match entity counts",
            "snapshot.integrity.acceleration"));
    }
    if (compute_acceleration_hash(snapshot->unit_bvh, snapshot->cover_bvh) !=
        snapshot->acceleration_hash) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale,
            "snapshot acceleration structure hash mismatch",
            "snapshot.integrity.acceleration_hash"));
    }
    return Outcome<void>::success();
}

Outcome<void> verify_standalone_snapshot(const PublishedRegionPtr& snapshot) {
    if (!snapshot) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale, "snapshot is empty",
            "snapshot.integrity"));
    }
    if (snapshot->state.compute_hash() != snapshot->state.content_hash) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale, "snapshot canonical hash mismatch",
            "snapshot.integrity.hash"));
    }
    if (snapshot->unit_bvh.primitive_bounds().size() != snapshot->state.units.size() ||
        snapshot->cover_bvh.primitive_bounds().size() != snapshot->state.covers.size()) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale,
            "snapshot acceleration structure does not match entity counts",
            "snapshot.integrity.acceleration"));
    }
    if (compute_acceleration_hash(snapshot->unit_bvh, snapshot->cover_bvh) !=
        snapshot->acceleration_hash) {
        return Outcome<void>::failure(query_error(
            StatusCode::SnapshotStale,
            "snapshot acceleration structure hash mismatch",
            "snapshot.integrity.acceleration_hash"));
    }
    return Outcome<void>::success();
}

bool better_candidate(const Candidate& candidate, const Candidate& current) {
    if (!candidate.found) {
        return false;
    }
    if (!current.found) {
        return true;
    }
    if (candidate.t < current.t) {
        return true;
    }
    if (candidate.t > current.t) {
        return false;
    }
    if (candidate.priority != current.priority) {
        return candidate.priority < current.priority;
    }
    if (candidate.entity.kind != current.entity.kind) {
        return static_cast<std::uint8_t>(candidate.entity.kind) <
               static_cast<std::uint8_t>(current.entity.kind);
    }
    return candidate.entity.id < current.entity.id;
}

void consider(Candidate& best, Candidate candidate, const Ray& ray) {
    if (candidate.found) {
        candidate.point = ray.origin + ray.direction * candidate.t;
    }
    if (better_candidate(candidate, best)) {
        best = candidate;
    }
}

bool intersect_triangle(const Ray& ray, const Triangle& triangle, float& t,
                        glm::vec3& normal) {
    const glm::vec3 edge1 = triangle.b - triangle.a;
    const glm::vec3 edge2 = triangle.c - triangle.a;
    const glm::vec3 pvec = glm::cross(ray.direction, edge2);
    const float determinant = glm::dot(edge1, pvec);
    if (!is_finite(determinant) || std::fabs(determinant) <= kIntersectEpsilon) {
        return false;
    }
    const float inverse = 1.0F / determinant;
    const glm::vec3 tvec = ray.origin - triangle.a;
    const float u = glm::dot(tvec, pvec) * inverse;
    if (!is_finite(u) || u < -kIntersectEpsilon || u > 1.0F + kIntersectEpsilon) {
        return false;
    }
    const glm::vec3 qvec = glm::cross(tvec, edge1);
    const float v = glm::dot(ray.direction, qvec) * inverse;
    if (!is_finite(v) || v < -kIntersectEpsilon ||
        u + v > 1.0F + kIntersectEpsilon) {
        return false;
    }
    const float candidate_t = glm::dot(edge2, qvec) * inverse;
    if (!is_finite(candidate_t) || candidate_t < 0.0F) {
        return false;
    }
    glm::vec3 surface_normal = glm::cross(edge1, edge2);
    const float length = glm::length(surface_normal);
    if (!is_finite(length) || length <= kIntersectEpsilon) {
        return false;
    }
    surface_normal /= length;
    if (glm::dot(surface_normal, ray.direction) > 0.0F) {
        surface_normal = -surface_normal;
    }
    t = candidate_t;
    normal = surface_normal;
    return true;
}

bool intersect_capsule(const Ray& ray, const Capsule& capsule, float& t,
                       glm::vec3& normal) {
    if (!valid_capsule(capsule)) {
        return false;
    }
    const glm::vec3 ba = capsule.b - capsule.a;
    const float length = glm::length(ba);
    if (!is_finite(length) || length <= kIntersectEpsilon) {
        return false;
    }
    const glm::vec3 axis = ba / length;
    const glm::vec3 oa = ray.origin - capsule.a;
    const float radius_squared = capsule.radius * capsule.radius;
    float best_t = std::numeric_limits<float>::infinity();
    glm::vec3 best_normal(0.0F);

    const glm::vec3 direction_perp =
        ray.direction - axis * glm::dot(ray.direction, axis);
    const glm::vec3 origin_perp = oa - axis * glm::dot(oa, axis);
    const float qa = glm::dot(direction_perp, direction_perp);
    if (qa > kIntersectEpsilon * kIntersectEpsilon) {
        const float qb = 2.0F * glm::dot(origin_perp, direction_perp);
        const float qc = glm::dot(origin_perp, origin_perp) - radius_squared;
        const float discriminant = qb * qb - 4.0F * qa * qc;
        if (is_finite(discriminant) && discriminant >= 0.0F) {
            const float root = std::sqrt(discriminant);
            const float roots[2] = {
                (-qb - root) / (2.0F * qa),
                (-qb + root) / (2.0F * qa)};
            for (float candidate_t : roots) {
                if (!is_finite(candidate_t) || candidate_t < 0.0F ||
                    candidate_t >= best_t) {
                    continue;
                }
                const glm::vec3 point = ray.origin + ray.direction * candidate_t;
                const float axial = glm::dot(point - capsule.a, axis);
                if (axial < 0.0F || axial > length) {
                    continue;
                }
                const glm::vec3 center = capsule.a + axis * axial;
                glm::vec3 candidate_normal = point - center;
                const float normal_length = glm::length(candidate_normal);
                if (!is_finite(normal_length) || normal_length <= kIntersectEpsilon) {
                    continue;
                }
                best_t = candidate_t;
                best_normal = candidate_normal / normal_length;
            }
        }
    }

    const glm::vec3 centers[2] = {capsule.a, capsule.b};
    for (int cap = 0; cap < 2; ++cap) {
        const glm::vec3 center = centers[cap];
        const glm::vec3 oc = ray.origin - center;
        const float qb = 2.0F * glm::dot(oc, ray.direction);
        const float qc = glm::dot(oc, oc) - radius_squared;
        const float discriminant = qb * qb - 4.0F * qc;
        if (!is_finite(discriminant) || discriminant < 0.0F) {
            continue;
        }
        const float root = std::sqrt(discriminant);
        const float roots[2] = {(-qb - root) * 0.5F, (-qb + root) * 0.5F};
        for (float candidate_t : roots) {
            if (!is_finite(candidate_t) || candidate_t < 0.0F ||
                candidate_t >= best_t) {
                continue;
            }
            const glm::vec3 point = ray.origin + ray.direction * candidate_t;
            const float side = glm::dot(point - center, axis);
            if ((cap == 0 && side > kIntersectEpsilon) ||
                (cap == 1 && side < -kIntersectEpsilon)) {
                continue;
            }
            glm::vec3 candidate_normal = point - center;
            const float normal_length = glm::length(candidate_normal);
            if (!is_finite(normal_length) || normal_length <= kIntersectEpsilon) {
                continue;
            }
            best_t = candidate_t;
            best_normal = candidate_normal / normal_length;
        }
    }

    if (!std::isfinite(best_t)) {
        return false;
    }
    t = best_t;
    normal = best_normal;
    return true;
}

bool intersect_unit(const Ray& ray, const Unit& unit, float& t, glm::vec3& normal) {
    if (unit.shape == UnitShape::Aabb) {
        float far_t = 0.0F;
        return ray_aabb(ray, unit.aabb, t, far_t, normal);
    }
    return intersect_capsule(ray, unit.capsule, t, normal);
}

bool intersect_ground(const Ray& ray, const Grid& grid, float& t,
                      glm::vec3& normal) {
    if (!is_finite(ray.origin) || !is_finite(ray.direction)) {
        return false;
    }
    if (std::fabs(ray.direction.y) <= kIntersectEpsilon) {
        return false;
    }
    const float candidate_t = (grid.ground_y() - ray.origin.y) / ray.direction.y;
    if (!is_finite(candidate_t) || candidate_t < 0.0F) {
        return false;
    }
    const glm::vec3 point = ray.origin + ray.direction * candidate_t;
    if (!grid.in_bounds_xz(point.x, point.z)) {
        return false;
    }
    t = candidate_t;
    normal = glm::vec3(0.0F, 1.0F, 0.0F);
    return true;
}

Outcome<PickResult> calculate_pick(const PublishedRegionPtr& snapshot, Ray ray,
                                   const Camera* camera,
                                   const ScreenPoint* screen) {
    Candidate best;

    snapshot->unit_bvh.query(ray, [&](std::uint32_t primitive) {
        if (primitive >= snapshot->state.units.size()) {
            return;
        }
        const Unit& unit = snapshot->state.units[primitive];
        Candidate candidate;
        candidate.entity = {EntityKind::Unit, unit.id};
        candidate.priority = 0;
        candidate.found = intersect_unit(
            ray, unit, candidate.t, candidate.normal);
        consider(best, candidate, ray);
    });

    snapshot->cover_bvh.query(ray, [&](std::uint32_t primitive) {
        if (primitive >= snapshot->state.covers.size()) {
            return;
        }
        const Cover& cover = snapshot->state.covers[primitive];
        if (!cover.present) {
            return;
        }
        Candidate candidate;
        candidate.entity = {EntityKind::Cover, cover.id};
        candidate.priority = 1;
        candidate.found = intersect_triangle(
            ray, cover.triangle, candidate.t, candidate.normal);
        consider(best, candidate, ray);
    });

    {
        Candidate ground;
        ground.entity = {EntityKind::Ground, 0};
        ground.priority = 2;
        ground.found = intersect_ground(
            ray, snapshot->state.grid, ground.t, ground.normal);
        consider(best, ground, ray);
    }

    PickResult result;
    result.region_version = snapshot->state.version;
    result.region_hash = snapshot->state.content_hash;
    result.camera = camera != nullptr ? *camera : Camera{};
    if (screen != nullptr) {
        result.screen = *screen;
    }
    result.ray = ray;
    result.has_hit = best.found;
    if (best.found) {
        result.hit.entity = best.entity;
        result.hit.point = ray.origin + ray.direction * best.t;
        result.hit.normal = best.normal;
        result.hit.distance = best.t;
    }
    return Outcome<PickResult>::success(std::move(result));
}

}  // namespace

Outcome<PublishedRegionPtr> validate_query_snapshot(
    const RegionStore& store, const SnapshotHandle& handle,
    std::uint64_t expected_version, const std::string& expected_hash) {
    Outcome<PublishedRegionPtr> resolved = store.resolve(handle);
    if (!resolved) {
        return resolved;
    }
    Outcome<void> integrity = verify_snapshot_integrity(resolved.value(), handle);
    if (!integrity) {
        return Outcome<PublishedRegionPtr>::failure(integrity.error());
    }
    if (expected_version != 0 &&
        resolved.value()->state.version != expected_version) {
        return Outcome<PublishedRegionPtr>::failure(query_error(
            StatusCode::RegionStale,
            "query expected version does not match snapshot",
            "region.version"));
    }
    if (!expected_hash.empty() &&
        resolved.value()->state.content_hash != expected_hash) {
        return Outcome<PublishedRegionPtr>::failure(query_error(
            StatusCode::RegionStale, "query expected hash does not match snapshot",
            "region.hash"));
    }
    return resolved;
}

Outcome<PickResult> pick_screen(const RegionStore& store,
                                const SnapshotHandle& handle,
                                const PickRequest& request) {
    Outcome<PublishedRegionPtr> snapshot = validate_query_snapshot(
        store, handle, request.expected_version, request.expected_hash);
    if (!snapshot) {
        return Outcome<PickResult>::failure(snapshot.error());
    }
    Outcome<Ray> ray = screen_to_ray(request.camera, request.screen);
    if (!ray) {
        return Outcome<PickResult>::failure(ray.error());
    }
    return calculate_pick(snapshot.value(), ray.value(), &request.camera,
                          &request.screen);
}

Outcome<PickResult> pick_screen_snapshot(const PublishedRegionPtr& snapshot,
                                         const PickRequest& request) {
    Outcome<void> integrity = verify_standalone_snapshot(snapshot);
    if (!integrity) {
        return Outcome<PickResult>::failure(integrity.error());
    }
    if (request.expected_version != 0 &&
        snapshot->state.version != request.expected_version) {
        return Outcome<PickResult>::failure(query_error(
            StatusCode::RegionStale,
            "query expected version does not match snapshot",
            "region.version"));
    }
    if (!request.expected_hash.empty() &&
        snapshot->state.content_hash != request.expected_hash) {
        return Outcome<PickResult>::failure(query_error(
            StatusCode::RegionStale, "query expected hash does not match snapshot",
            "region.hash"));
    }
    Outcome<Ray> ray = screen_to_ray(request.camera, request.screen);
    if (!ray) {
        return Outcome<PickResult>::failure(ray.error());
    }
    return calculate_pick(snapshot, ray.value(), &request.camera,
                          &request.screen);
}

Outcome<PickResult> pick_ray(const RegionStore& store,
                             const SnapshotHandle& handle, Ray ray,
                             std::uint64_t expected_version,
                             const std::string& expected_hash) {
    Outcome<Ray> validated = validate_ray(ray, "ray");
    if (!validated) {
        return Outcome<PickResult>::failure(validated.error());
    }
    Outcome<PublishedRegionPtr> snapshot = validate_query_snapshot(
        store, handle, expected_version, expected_hash);
    if (!snapshot) {
        return Outcome<PickResult>::failure(snapshot.error());
    }
    return calculate_pick(snapshot.value(), validated.value(), nullptr, nullptr);
}

Outcome<BoxSelectResult> box_select(const RegionStore& store,
                                    const SnapshotHandle& handle,
                                    const BoxSelectRequest& request) {
    Outcome<PublishedRegionPtr> snapshot = validate_query_snapshot(
        store, handle, request.expected_version, request.expected_hash);
    if (!snapshot) {
        return Outcome<BoxSelectResult>::failure(snapshot.error());
    }
    Outcome<CameraBasis> camera = validate_camera(request.camera);
    if (!camera) {
        return Outcome<BoxSelectResult>::failure(camera.error());
    }
    if (!is_finite(request.rect.min_x) || !is_finite(request.rect.min_y) ||
        !is_finite(request.rect.max_x) || !is_finite(request.rect.max_y)) {
        return Outcome<BoxSelectResult>::failure(query_error(
            StatusCode::RayInvalid, "selection rectangle must be finite",
            "box_select.rect.nonfinite"));
    }
    if (request.rect.min_x < 0.0F || request.rect.min_y < 0.0F ||
        request.rect.max_x > request.camera.viewport_width ||
        request.rect.max_y > request.camera.viewport_height ||
        request.rect.max_x < request.rect.min_x ||
        request.rect.max_y < request.rect.min_y) {
        return Outcome<BoxSelectResult>::failure(query_error(
            StatusCode::RayInvalid, "selection rectangle is outside the viewport",
            "box_select.rect.bounds"));
    }

    BoxSelectResult result;
    result.region_version = snapshot.value()->state.version;
    result.region_hash = snapshot.value()->state.content_hash;
    for (const Unit& unit : snapshot.value()->state.units) {
        const Aabb bounds = unit_bounds(unit);
        const glm::vec3 corners[8] = {
            {bounds.min.x, bounds.min.y, bounds.min.z},
            {bounds.max.x, bounds.min.y, bounds.min.z},
            {bounds.min.x, bounds.max.y, bounds.min.z},
            {bounds.max.x, bounds.max.y, bounds.min.z},
            {bounds.min.x, bounds.min.y, bounds.max.z},
            {bounds.max.x, bounds.min.y, bounds.max.z},
            {bounds.min.x, bounds.max.y, bounds.max.z},
            {bounds.max.x, bounds.max.y, bounds.max.z}};
        float min_x = std::numeric_limits<float>::infinity();
        float min_y = std::numeric_limits<float>::infinity();
        float max_x = -std::numeric_limits<float>::infinity();
        float max_y = -std::numeric_limits<float>::infinity();
        bool projected = false;
        for (const glm::vec3& corner : corners) {
            Outcome<glm::vec2> screen = project_to_screen(request.camera, corner);
            if (!screen) {
                continue;
            }
            projected = true;
            min_x = std::min(min_x, screen.value().x);
            min_y = std::min(min_y, screen.value().y);
            max_x = std::max(max_x, screen.value().x);
            max_y = std::max(max_y, screen.value().y);
        }
        if (!projected) {
            continue;
        }
        const bool overlaps =
            max_x >= request.rect.min_x && min_x <= request.rect.max_x &&
            max_y >= request.rect.min_y && min_y <= request.rect.max_y;
        if (overlaps) {
            result.unit_ids.push_back(unit.id);
        }
    }
    std::sort(result.unit_ids.begin(), result.unit_ids.end());
    return Outcome<BoxSelectResult>::success(std::move(result));
}

}  // namespace battle
