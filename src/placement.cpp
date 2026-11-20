#include "battle/placement.h"

#include "battle/query.h"

#include <glm/geometric.hpp>

#include <algorithm>
#include <cmath>
#include <limits>
#include <set>
#include <sstream>

namespace battle {
namespace {

constexpr std::size_t kMaxVertices = 256;
constexpr float kGeometryEpsilon = 1.0e-6F;

struct Point2 {
    float x = 0.0F;
    float y = 0.0F;
};

bool point_in_triangle(const Point2& point, const Point2 triangle[3]);

Error placement_error(std::string message, std::string point,
                      int vertex = -1, int edge = -1) {
    Error error;
    error.code = StatusCode::RegionInvalid;
    error.message = std::move(message);
    error.injection_point = std::move(point);
    error.vertex_index = vertex;
    error.edge_index = edge;
    return error;
}

float cross2(const Point2& a, const Point2& b, const Point2& c) {
    return (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x);
}

bool point_on_segment(const Point2& point, const Point2& a, const Point2& b) {
    const float area = cross2(a, b, point);
    if (std::fabs(area) > kGeometryEpsilon) {
        return false;
    }
    return point.x >= std::min(a.x, b.x) - kGeometryEpsilon &&
           point.x <= std::max(a.x, b.x) + kGeometryEpsilon &&
           point.y >= std::min(a.y, b.y) - kGeometryEpsilon &&
           point.y <= std::max(a.y, b.y) + kGeometryEpsilon;
}

bool segments_intersect(const Point2& a, const Point2& b,
                        const Point2& c, const Point2& d) {
    const float ab_c = cross2(a, b, c);
    const float ab_d = cross2(a, b, d);
    const float cd_a = cross2(c, d, a);
    const float cd_b = cross2(c, d, b);
    if (((ab_c > kGeometryEpsilon && ab_d < -kGeometryEpsilon) ||
         (ab_c < -kGeometryEpsilon && ab_d > kGeometryEpsilon)) &&
        ((cd_a > kGeometryEpsilon && cd_b < -kGeometryEpsilon) ||
         (cd_a < -kGeometryEpsilon && cd_b > kGeometryEpsilon))) {
        return true;
    }
    return point_on_segment(c, a, b) || point_on_segment(d, a, b) ||
           point_on_segment(a, c, d) || point_on_segment(b, c, d);
}

bool point_in_polygon(const Point2& point, const std::vector<Point2>& polygon) {
    bool inside = false;
    for (std::size_t i = 0, j = polygon.size() - 1; i < polygon.size(); j = i++) {
        const Point2& pi = polygon[i];
        const Point2& pj = polygon[j];
        if (point_on_segment(point, pj, pi)) {
            return true;
        }
        const bool crosses = ((pi.y > point.y) != (pj.y > point.y)) &&
                             (point.x < (pj.x - pi.x) * (point.y - pi.y) /
                                                (pj.y - pi.y) +
                                            pi.x);
        if (crosses) {
            inside = !inside;
        }
    }
    return inside;
}

bool point_in_rect(const Point2& point, float min_x, float min_y,
                   float max_x, float max_y) {
    return point.x >= min_x - kGeometryEpsilon &&
           point.x <= max_x + kGeometryEpsilon &&
           point.y >= min_y - kGeometryEpsilon &&
           point.y <= max_y + kGeometryEpsilon;
}

bool rect_edges_intersect_polygon(float min_x, float min_y,
                                  float max_x, float max_y,
                                  const std::vector<Point2>& polygon) {
    const Point2 corners[4] = {
        {min_x, min_y}, {max_x, min_y}, {max_x, max_y}, {min_x, max_y}};
    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const Point2& a = polygon[i];
        const Point2& b = polygon[(i + 1) % polygon.size()];
        for (int edge = 0; edge < 4; ++edge) {
            if (segments_intersect(a, b, corners[edge], corners[(edge + 1) % 4])) {
                return true;
            }
        }
    }
    return false;
}

bool rect_overlaps_polygon(float min_x, float min_y, float max_x, float max_y,
                           const std::vector<Point2>& polygon) {
    if (point_in_polygon({(min_x + max_x) * 0.5F, (min_y + max_y) * 0.5F},
                         polygon)) {
        return true;
    }
    for (const Point2& vertex : polygon) {
        if (point_in_rect(vertex, min_x, min_y, max_x, max_y)) {
            return true;
        }
    }
    return rect_edges_intersect_polygon(min_x, min_y, max_x, max_y, polygon);
}

bool triangle_overlaps_polygon(const Point2 triangle[3],
                               const std::vector<Point2>& polygon) {
    for (int i = 0; i < 3; ++i) {
        if (point_in_polygon(triangle[i], polygon)) {
            return true;
        }
    }
    for (const Point2& vertex : polygon) {
        if (point_on_segment(vertex, triangle[0], triangle[1]) ||
            point_on_segment(vertex, triangle[1], triangle[2]) ||
            point_on_segment(vertex, triangle[2], triangle[0]) ||
            point_in_triangle(vertex, triangle)) {
            return true;
        }
    }
    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const Point2& a = polygon[i];
        const Point2& b = polygon[(i + 1) % polygon.size()];
        for (int edge = 0; edge < 3; ++edge) {
            if (segments_intersect(a, b, triangle[edge],
                                   triangle[(edge + 1) % 3])) {
                return true;
            }
        }
    }
    return false;
}

bool point_in_triangle(const Point2& point, const Point2 triangle[3]) {
    const float c0 = cross2(triangle[0], triangle[1], point);
    const float c1 = cross2(triangle[1], triangle[2], point);
    const float c2 = cross2(triangle[2], triangle[0], point);
    const bool has_negative =
        c0 < -kGeometryEpsilon || c1 < -kGeometryEpsilon ||
        c2 < -kGeometryEpsilon;
    const bool has_positive =
        c0 > kGeometryEpsilon || c1 > kGeometryEpsilon ||
        c2 > kGeometryEpsilon;
    return !(has_negative && has_positive);
}

float point_segment_distance_squared(const Point2& point, const Point2& a,
                                     const Point2& b) {
    const float dx = b.x - a.x;
    const float dy = b.y - a.y;
    const float length_squared = dx * dx + dy * dy;
    if (length_squared <= kGeometryEpsilon * kGeometryEpsilon) {
        const float px = point.x - a.x;
        const float py = point.y - a.y;
        return px * px + py * py;
    }
    float t = ((point.x - a.x) * dx + (point.y - a.y) * dy) / length_squared;
    t = std::max(0.0F, std::min(1.0F, t));
    const float px = point.x - (a.x + t * dx);
    const float py = point.y - (a.y + t * dy);
    return px * px + py * py;
}

bool capsule_overlaps_polygon(const Point2& a, const Point2& b, float radius,
                              const std::vector<Point2>& polygon) {
    const float radius_squared = radius * radius;
    if (point_in_polygon(a, polygon) || point_in_polygon(b, polygon)) {
        return true;
    }
    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const Point2& p = polygon[i];
        const Point2& q = polygon[(i + 1) % polygon.size()];
        if (segments_intersect(a, b, p, q)) {
            return true;
        }
        if (point_segment_distance_squared(p, a, b) <= radius_squared) {
            return true;
        }
    }
    return false;
}

int nearest_vertex(const std::vector<Point2>& polygon, const Point2& point) {
    int best = -1;
    float best_distance = std::numeric_limits<float>::infinity();
    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const float dx = polygon[i].x - point.x;
        const float dy = polygon[i].y - point.y;
        const float distance = dx * dx + dy * dy;
        if (distance < best_distance) {
            best_distance = distance;
            best = static_cast<int>(i);
        }
    }
    return best;
}

void add_conflict(std::vector<EntityRef>& conflicts, EntityRef entity) {
    if (std::find(conflicts.begin(), conflicts.end(), entity) == conflicts.end()) {
        conflicts.push_back(entity);
    }
}

void sort_conflicts(std::vector<EntityRef>& conflicts) {
    std::sort(conflicts.begin(), conflicts.end());
}

bool unit_overlaps(const Unit& unit, float clearance,
                   const std::vector<Point2>& polygon) {
    if (unit.shape == UnitShape::Aabb) {
        return rect_overlaps_polygon(
            unit.aabb.min.x - clearance, unit.aabb.min.z - clearance,
            unit.aabb.max.x + clearance, unit.aabb.max.z + clearance, polygon);
    }
    return capsule_overlaps_polygon(
        {unit.capsule.a.x, unit.capsule.a.z},
        {unit.capsule.b.x, unit.capsule.b.z},
        unit.capsule.radius + clearance, polygon);
}

bool cover_overlaps(const Cover& cover, float clearance,
                    const std::vector<Point2>& polygon) {
    if (!cover.present) {
        return false;
    }
    const Point2 source[3] = {
        {cover.triangle.a.x, cover.triangle.a.z},
        {cover.triangle.b.x, cover.triangle.b.z},
        {cover.triangle.c.x, cover.triangle.c.z}};
    const Point2 centroid{
        (source[0].x + source[1].x + source[2].x) / 3.0F,
        (source[0].y + source[1].y + source[2].y) / 3.0F};
    Point2 triangle[3] = {source[0], source[1], source[2]};
    if (clearance > 0.0F) {
        for (Point2& vertex : triangle) {
            const float dx = vertex.x - centroid.x;
            const float dz = vertex.y - centroid.y;
            const float length = std::sqrt(dx * dx + dz * dz);
            if (length > kGeometryEpsilon) {
                vertex.x += dx / length * clearance;
                vertex.y += dz / length * clearance;
            }
        }
    }
    return triangle_overlaps_polygon(triangle, polygon);
}

}  // namespace

const char* placement_reason_name(PlacementReason reason) noexcept {
    switch (reason) {
        case PlacementReason::Legal: return "LEGAL";
        case PlacementReason::BlockedCell: return "BLOCKED_CELL";
        case PlacementReason::UnitOverlap: return "UNIT_OVERLAP";
        case PlacementReason::CoverOverlap: return "COVER_OVERLAP";
    }
    return "UNKNOWN";
}

Outcome<PlacementResult> validate_placement(const RegionStore& store,
                                            const SnapshotHandle& handle,
                                            const PlacementRequest& request) {
    Outcome<PublishedRegionPtr> snapshot = validate_query_snapshot(
        store, handle, request.expected_version, request.expected_hash);
    if (!snapshot) {
        return Outcome<PlacementResult>::failure(snapshot.error());
    }
    if (request.region.vertices.size() < 3 ||
        request.region.vertices.size() > kMaxVertices) {
        return Outcome<PlacementResult>::failure(placement_error(
            "draft region must contain between 3 and 256 vertices",
            "placement.vertices.count"));
    }
    if (!is_finite(request.unit_clearance) || !is_finite(request.cover_clearance) ||
        request.unit_clearance < 0.0F || request.cover_clearance < 0.0F) {
        return Outcome<PlacementResult>::failure(placement_error(
            "placement clearances must be finite and non-negative",
            "placement.clearance"));
    }

    const Grid& grid = snapshot.value()->state.grid;
    std::vector<Point2> polygon;
    polygon.reserve(request.region.vertices.size());
    for (std::size_t i = 0; i < request.region.vertices.size(); ++i) {
        const glm::vec3& vertex = request.region.vertices[i];
        if (!is_finite(vertex)) {
            return Outcome<PlacementResult>::failure(placement_error(
                "draft vertex must be finite", "placement.vertex.nonfinite",
                static_cast<int>(i)));
        }
        if (std::fabs(vertex.y - grid.ground_y()) > kGeometryEpsilon) {
            return Outcome<PlacementResult>::failure(placement_error(
                "draft vertex is not on the ground plane",
                "placement.vertex.plane", static_cast<int>(i)));
        }
        if (!grid.in_bounds_xz(vertex.x, vertex.z)) {
            return Outcome<PlacementResult>::failure(placement_error(
                "draft vertex is outside the grid",
                "placement.vertex.bounds", static_cast<int>(i)));
        }
        if (!grid.walkable_world_xz(vertex.x, vertex.z)) {
            // This becomes a normal placement rejection below, but keeping
            // the vertex here gives exact attribution.
        }
        polygon.push_back({vertex.x, vertex.z});
    }

    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const std::size_t next = (i + 1) % polygon.size();
        const float dx = polygon[next].x - polygon[i].x;
        const float dy = polygon[next].y - polygon[i].y;
        if (dx * dx + dy * dy <= kGeometryEpsilon * kGeometryEpsilon) {
            return Outcome<PlacementResult>::failure(placement_error(
                "draft region has a zero-length edge",
                "placement.vertex.duplicate", static_cast<int>(i),
                static_cast<int>(i)));
        }
    }

    float area_twice = 0.0F;
    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const Point2& a = polygon[i];
        const Point2& b = polygon[(i + 1) % polygon.size()];
        area_twice += a.x * b.y - b.x * a.y;
    }
    if (std::fabs(area_twice) <= kGeometryEpsilon) {
        return Outcome<PlacementResult>::failure(placement_error(
            "draft region has zero area", "placement.area", 0, 0));
    }

    for (std::size_t i = 0; i < polygon.size(); ++i) {
        const std::size_t i_next = (i + 1) % polygon.size();
        for (std::size_t j = i + 1; j < polygon.size(); ++j) {
            const std::size_t j_next = (j + 1) % polygon.size();
            if (i == j || i_next == j || j_next == i) {
                continue;
            }
            if (segments_intersect(polygon[i], polygon[i_next],
                                   polygon[j], polygon[j_next])) {
                return Outcome<PlacementResult>::failure(placement_error(
                    "draft region is self-intersecting",
                    "placement.edge.self_intersection", static_cast<int>(i),
                    static_cast<int>(j)));
            }
        }
    }

    PlacementResult result;
    result.region_version = snapshot.value()->state.version;
    result.region_hash = snapshot.value()->state.content_hash;
    result.legal = true;

    float min_x = polygon[0].x;
    float max_x = polygon[0].x;
    float min_z = polygon[0].y;
    float max_z = polygon[0].y;
    for (const Point2& point : polygon) {
        min_x = std::min(min_x, point.x);
        max_x = std::max(max_x, point.x);
        min_z = std::min(min_z, point.y);
        max_z = std::max(max_z, point.y);
    }

    auto cell_index = [&](float coordinate, float origin, std::uint32_t count) {
        float index = (coordinate - origin) / grid.cell_size;
        index = std::max(0.0F, std::min(index, static_cast<float>(count - 1)));
        return static_cast<std::uint32_t>(std::floor(index));
    };
    const std::uint32_t cell_min_x = cell_index(min_x, grid.origin.x, grid.width);
    const std::uint32_t cell_max_x = cell_index(max_x, grid.origin.x, grid.width);
    const std::uint32_t cell_min_z = cell_index(min_z, grid.origin.z, grid.depth);
    const std::uint32_t cell_max_z = cell_index(max_z, grid.origin.z, grid.depth);

    for (std::uint32_t z = cell_min_z; z <= cell_max_z; ++z) {
        for (std::uint32_t x = cell_min_x; x <= cell_max_x; ++x) {
            const float cell_x0 = grid.origin.x + static_cast<float>(x) * grid.cell_size;
            const float cell_z0 = grid.origin.z + static_cast<float>(z) * grid.cell_size;
            const float cell_x1 = cell_x0 + grid.cell_size;
            const float cell_z1 = cell_z0 + grid.cell_size;
            if (!rect_overlaps_polygon(cell_x0, cell_z0, cell_x1, cell_z1,
                                       polygon)) {
                continue;
            }
            if (!grid.walkable_at(x, 0, z)) {
                result.legal = false;
                result.reason = PlacementReason::BlockedCell;
                result.failed_cell_x = x;
                result.failed_cell_z = z;
                result.failed_vertex = nearest_vertex(
                    polygon, {(cell_x0 + cell_x1) * 0.5F,
                              (cell_z0 + cell_z1) * 0.5F});
                std::ostringstream message;
                message << "blocked grid cell at (" << x << ",0," << z << ")";
                result.detail = message.str();
                return Outcome<PlacementResult>::success(std::move(result));
            }
        }
    }

    for (const Unit& unit : snapshot.value()->state.units) {
        if (!unit_overlaps(unit, request.unit_clearance, polygon)) {
            continue;
        }
        result.legal = false;
        result.reason = PlacementReason::UnitOverlap;
        add_conflict(result.conflicts, {EntityKind::Unit, unit.id});
        const Aabb bounds = unit_bounds(unit);
        const int vertex = nearest_vertex(
            polygon, {(bounds.min.x + bounds.max.x) * 0.5F,
                      (bounds.min.z + bounds.max.z) * 0.5F});
        if (result.failed_vertex < 0) {
            result.failed_vertex = vertex;
        }
    }
    if (!result.legal) {
        sort_conflicts(result.conflicts);
        result.detail = "draft region overlaps one or more units";
        return Outcome<PlacementResult>::success(std::move(result));
    }

    for (const Cover& cover : snapshot.value()->state.covers) {
        if (!cover_overlaps(cover, request.cover_clearance, polygon)) {
            continue;
        }
        result.legal = false;
        result.reason = PlacementReason::CoverOverlap;
        add_conflict(result.conflicts, {EntityKind::Cover, cover.id});
        const glm::vec3 center =
            (cover.triangle.a + cover.triangle.b + cover.triangle.c) / 3.0F;
        const int vertex = nearest_vertex(polygon, {center.x, center.z});
        if (result.failed_vertex < 0) {
            result.failed_vertex = vertex;
        }
    }
    if (!result.legal) {
        sort_conflicts(result.conflicts);
        result.detail = "draft region intersects one or more covers";
        return Outcome<PlacementResult>::success(std::move(result));
    }

    result.reason = PlacementReason::Legal;
    result.detail = "draft region is legal";
    return Outcome<PlacementResult>::success(std::move(result));
}

}  // namespace battle
