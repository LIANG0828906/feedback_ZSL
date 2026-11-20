#include "core.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <set>
#include <sstream>
#include <tuple>

namespace fill {

Vec2 operator+(Vec2 a, Vec2 b) { return {a.x + b.x, a.y + b.y}; }
Vec2 operator-(Vec2 a, Vec2 b) { return {a.x - b.x, a.y - b.y}; }
Vec2 operator*(Vec2 a, double s) { return {a.x * s, a.y * s}; }
Vec2 operator/(Vec2 a, double s) { return {a.x / s, a.y / s}; }
double dot(Vec2 a, Vec2 b) { return a.x * b.x + a.y * b.y; }
double cross(Vec2 a, Vec2 b) { return a.x * b.y - a.y * b.x; }
double length(Vec2 v) { return std::hypot(v.x, v.y); }
double distance(Vec2 a, Vec2 b) { return length(a - b); }
bool finite(Vec2 v) { return std::isfinite(v.x) && std::isfinite(v.y); }

void Bounds::include(Vec2 p) {
    if (!valid) {
        minX = maxX = p.x;
        minY = maxY = p.y;
        valid = true;
    } else {
        minX = std::min(minX, p.x);
        minY = std::min(minY, p.y);
        maxX = std::max(maxX, p.x);
        maxY = std::max(maxY, p.y);
    }
}

double Bounds::width() const { return valid ? maxX - minX : 0.0; }
double Bounds::height() const { return valid ? maxY - minY : 0.0; }

namespace {

double pointLineDistance(Vec2 p, Vec2 a, Vec2 b) {
    const Vec2 ab = b - a;
    const double denominator = dot(ab, ab);
    if (denominator <= 0.0) {
        return distance(p, a);
    }
    const double t = dot(p - a, ab) / denominator;
    const double clamped = std::max(0.0, std::min(1.0, t));
    return distance(p, a + ab * clamped);
}

void flattenQuadratic(Vec2 p0, Vec2 c, Vec2 p1, double tolerance, int depth,
                      std::vector<Vec2>& points) {
    const double flatness = pointLineDistance(c, p0, p1);
    if (flatness <= tolerance || depth >= 16) {
        points.push_back(p1);
        return;
    }
    const Vec2 p01 = (p0 + c) * 0.5;
    const Vec2 p12 = (c + p1) * 0.5;
    const Vec2 mid = (p01 + p12) * 0.5;
    flattenQuadratic(p0, p01, mid, tolerance, depth + 1, points);
    flattenQuadratic(mid, p12, p1, tolerance, depth + 1, points);
}

void flattenCubic(Vec2 p0, Vec2 c1, Vec2 c2, Vec2 p1, double tolerance,
                  int depth, std::vector<Vec2>& points) {
    const double flatness =
        std::max(pointLineDistance(c1, p0, p1), pointLineDistance(c2, p0, p1));
    if (flatness <= tolerance || depth >= 16) {
        points.push_back(p1);
        return;
    }
    const Vec2 p01 = (p0 + c1) * 0.5;
    const Vec2 p12 = (c1 + c2) * 0.5;
    const Vec2 p23 = (c2 + p1) * 0.5;
    const Vec2 p012 = (p01 + p12) * 0.5;
    const Vec2 p123 = (p12 + p23) * 0.5;
    const Vec2 mid = (p012 + p123) * 0.5;
    flattenCubic(p0, p01, p012, mid, tolerance, depth + 1, points);
    flattenCubic(mid, p123, p23, p1, tolerance, depth + 1, points);
}

struct Hit {
    bool intersects = false;
    bool parallel = false;
    double t = 0.0;
    double u = 0.0;
    Vec2 point;
};

Hit segmentIntersection(Vec2 a, Vec2 b, Vec2 c, Vec2 d, double tolerance) {
    Hit hit;
    const Vec2 r = b - a;
    const Vec2 s = d - c;
    const double denominator = cross(r, s);
    const double scale = std::max(1.0, length(r) * length(s));
    const double parameterTolerance =
        tolerance / std::max(1.0, length(r)) +
        tolerance / std::max(1.0, length(s));

    if (std::abs(denominator) <=
        std::numeric_limits<double>::epsilon() * scale * 32.0) {
        hit.parallel = true;
        if (std::abs(cross(c - a, r)) >
            tolerance * std::max(1.0, length(r))) {
            return hit;
        }
        const double rr = dot(r, r);
        if (rr <= 0.0) {
            return hit;
        }
        const double t0 = dot(c - a, r) / rr;
        const double t1 = dot(d - a, r) / rr;
        const double lo = std::max(0.0, std::min(t0, t1));
        const double hi = std::min(1.0, std::max(t0, t1));
        if (lo <= hi + parameterTolerance) {
            hit.intersects = true;
            hit.t = std::max(0.0, std::min(1.0, (lo + hi) * 0.5));
            hit.u = 0.5;
            hit.point = a + r * hit.t;
        }
        return hit;
    }

    const Vec2 delta = c - a;
    hit.t = cross(delta, s) / denominator;
    hit.u = cross(delta, r) / denominator;
    if (hit.t < -parameterTolerance || hit.t > 1.0 + parameterTolerance ||
        hit.u < -parameterTolerance || hit.u > 1.0 + parameterTolerance) {
        return hit;
    }
    hit.intersects = true;
    const double tc = std::max(0.0, std::min(1.0, hit.t));
    const double uc = std::max(0.0, std::min(1.0, hit.u));
    hit.point = (a + r * tc + c + s * uc) * 0.5;
    return hit;
}

std::string pointKey(Vec2 p, double tolerance) {
    const double grid = std::max(tolerance, 1e-12);
    const long long x = static_cast<long long>(std::llround(p.x / grid));
    const long long y = static_cast<long long>(std::llround(p.y / grid));
    return std::to_string(x) + ":" + std::to_string(y);
}

double polygonSignedArea(const std::vector<Vec2>& points) {
    double area = 0.0;
    if (points.size() < 3) {
        return 0.0;
    }
    for (std::size_t i = 0; i < points.size(); ++i) {
        area += cross(points[i], points[(i + 1) % points.size()]);
    }
    return area * 0.5;
}

}  // namespace

FlatPath flattenPath(const Path& path, double tolerance) {
    FlatPath flat;
    const double safeTolerance =
        std::max(tolerance, std::numeric_limits<double>::epsilon() * 16.0);
    for (std::size_t contourIndex = 0; contourIndex < path.contours.size();
         ++contourIndex) {
        const Contour& contour = path.contours[contourIndex];
        if (contour.segments.empty()) {
            flat.contourVertices.emplace_back();
            continue;
        }
        std::vector<Vec2> vertices;
        const Vec2 first = contour.segments.front().p0;
        vertices.push_back(first);
        for (const Segment& segment : contour.segments) {
            const std::size_t begin = vertices.size();
            if (distance(vertices.back(), segment.p0) > safeTolerance) {
                vertices.push_back(segment.p0);
            }
            switch (segment.type) {
                case SegmentType::Line: {
                    vertices.push_back(segment.p1);
                    break;
                }
                case SegmentType::Quadratic:
                    flattenQuadratic(segment.p0, segment.c1, segment.p1,
                                     safeTolerance, 0, vertices);
                    break;
                case SegmentType::Cubic:
                    flattenCubic(segment.p0, segment.c1, segment.c2, segment.p1,
                                 safeTolerance, 0, vertices);
                    break;
            }
            for (std::size_t pointIndex = std::max<std::size_t>(begin, 1);
                 pointIndex < vertices.size(); ++pointIndex) {
                if (distance(vertices[pointIndex - 1], vertices[pointIndex]) >
                    std::numeric_limits<double>::epsilon()) {
                    flat.edges.push_back(
                        {vertices[pointIndex - 1], vertices[pointIndex],
                         static_cast<int>(contourIndex), segment.sourceIndex});
                }
            }
        }
        flat.contourVertices.push_back(std::move(vertices));
    }
    return flat;
}

Diagnostics diagnosePath(const Path& path, const FlatPath& flat,
                         double tolerance) {
    Diagnostics diagnostics;
    diagnostics.monotonicSegmentCount =
        static_cast<int>(flat.edges.size());
    const double safeTolerance =
        std::max(tolerance, std::numeric_limits<double>::epsilon() * 16.0);

    std::set<std::string> intersectionKeys;
    for (std::size_t i = 0; i < flat.edges.size(); ++i) {
        const FlatEdge& a = flat.edges[i];
        for (std::size_t j = i + 1; j < flat.edges.size(); ++j) {
            const FlatEdge& b = flat.edges[j];
            if (a.contour == b.contour && a.segment == b.segment) {
                continue;
            }
            const Hit hit =
                segmentIntersection(a.a, a.b, b.a, b.b, safeTolerance);
            if (!hit.intersects) {
                continue;
            }
            if (a.contour == b.contour &&
                ((distance(hit.point, a.a) <= safeTolerance &&
                  distance(hit.point, b.a) <= safeTolerance) ||
                 (distance(hit.point, a.b) <= safeTolerance &&
                  distance(hit.point, b.a) <= safeTolerance) ||
                 (distance(hit.point, a.a) <= safeTolerance &&
                  distance(hit.point, b.b) <= safeTolerance) ||
                 (distance(hit.point, a.b) <= safeTolerance &&
                  distance(hit.point, b.b) <= safeTolerance))) {
                continue;
            }
            if (a.contour == b.contour && distance(a.a, b.a) <= safeTolerance &&
                distance(a.b, b.b) <= safeTolerance) {
                continue;
            }
            const std::string key = pointKey(hit.point, safeTolerance);
            if (!intersectionKeys.insert(key).second) {
                continue;
            }
            diagnostics.intersections.push_back(
                {hit.point, a.contour, a.segment, b.contour, b.segment,
                 hit.parallel ? "overlap" : "cross"});
        }
    }

    std::set<std::tuple<int, int, int, int>> pairKeys;
    for (std::size_t i = 0; i < flat.edges.size(); ++i) {
        const FlatEdge& a = flat.edges[i];
        for (std::size_t j = i + 1; j < flat.edges.size(); ++j) {
            const FlatEdge& b = flat.edges[j];
            if (a.contour == b.contour && a.segment == b.segment) {
                continue;
            }
            const Vec2 ra = a.b - a.a;
            const Vec2 rb = b.b - b.a;
            const double la = length(ra);
            const double lb = length(rb);
            if (la <= safeTolerance || lb <= safeTolerance) {
                continue;
            }
            const Vec2 ua = ra / la;
            const Vec2 ub = rb / lb;
            if (std::abs(dot(ua, ub)) < 0.98) {
                continue;
            }
            const double d1 = pointLineDistance(a.a, b.a, b.b);
            const double d2 = pointLineDistance(a.b, b.a, b.b);
            const double d3 = pointLineDistance(b.a, a.a, a.b);
            const double d4 = pointLineDistance(b.b, a.a, a.b);
            const double maxDistance = std::max({d1, d2, d3, d4});
            if (maxDistance > safeTolerance * 2.0) {
                continue;
            }
            int ca = a.contour;
            int sa = a.segment;
            int cb = b.contour;
            int sb = b.segment;
            if (std::tie(cb, sb) < std::tie(ca, sa)) {
                std::swap(ca, cb);
                std::swap(sa, sb);
            }
            if (!pairKeys.emplace(ca, sa, cb, sb).second) {
                continue;
            }
            const std::string confidence =
                maxDistance <= safeTolerance * 0.5
                    ? "high"
                    : (maxDistance <= safeTolerance ? "medium" : "low");
            diagnostics.nearCoincident.push_back(
                {ca, sa, cb, sb, dot(ua, ub) >= 0.0 ? "same" : "opposite",
                 confidence, maxDistance});
        }
    }

    const double shortThreshold = path.budget.vertexMerge;
    for (std::size_t ci = 0; ci < path.contours.size(); ++ci) {
        for (const Segment& segment : path.contours[ci].segments) {
            double measure = distance(segment.p0, segment.p1);
            if (segment.type == SegmentType::Quadratic) {
                measure = distance(segment.p0, segment.c1) +
                          distance(segment.c1, segment.p1);
            } else if (segment.type == SegmentType::Cubic) {
                measure = distance(segment.p0, segment.c1) +
                          distance(segment.c1, segment.c2) +
                          distance(segment.c2, segment.p1);
            }
            if (measure <= shortThreshold) {
                diagnostics.degenerate.push_back(
                    {static_cast<int>(ci), segment.sourceIndex,
                     "length_below_threshold", measure});
            }
        }
    }
    return diagnostics;
}

ViewTransform makeViewTransform(const Path& path, int width, int height) {
    ViewTransform transform;
    if (width <= 0 || height <= 0 || !path.bounds.valid) {
        return transform;
    }
    const double contentWidth = std::max(path.bounds.width(), 1e-12);
    const double contentHeight = std::max(path.bounds.height(), 1e-12);
    const double usableWidth = std::max(1.0, static_cast<double>(width) - 2.0);
    const double usableHeight = std::max(1.0, static_cast<double>(height) - 2.0);
    transform.scale =
        std::min(usableWidth / contentWidth, usableHeight / contentHeight);
    if (!std::isfinite(transform.scale) || transform.scale <= 0.0) {
        transform.scale = 1.0;
    }
    transform.offsetX =
        (static_cast<double>(width) - path.bounds.width() * transform.scale) *
        0.5;
    transform.offsetY =
        (static_cast<double>(height) - path.bounds.height() * transform.scale) *
        0.5;
    transform.worldPerPixel = 1.0 / transform.scale;
    return transform;
}

double contourSignedAreaWorld(const Contour& contour) {
    std::vector<Vec2> points;
    points.reserve(contour.segments.size() + 1);
    if (contour.segments.empty()) {
        return 0.0;
    }
    points.push_back(contour.segments.front().p0);
    for (const Segment& segment : contour.segments) {
        if (segment.type == SegmentType::Line) {
            points.push_back(segment.p1);
        } else if (segment.type == SegmentType::Quadratic) {
            for (int step = 1; step <= 16; ++step) {
                const double t = static_cast<double>(step) / 16.0;
                const double mt = 1.0 - t;
                points.push_back(segment.p0 * (mt * mt) +
                                 segment.c1 * (2.0 * mt * t) +
                                 segment.p1 * (t * t));
            }
        } else {
            for (int step = 1; step <= 24; ++step) {
                const double t = static_cast<double>(step) / 24.0;
                const double mt = 1.0 - t;
                points.push_back(segment.p0 * (mt * mt * mt) +
                                 segment.c1 * (3.0 * mt * mt * t) +
                                 segment.c2 * (3.0 * mt * t * t) +
                                 segment.p1 * (t * t * t));
            }
        }
    }
    return polygonSignedArea(points);
}

bool pointInPolygon(Vec2 point, const std::vector<Vec2>& polygon) {
    bool inside = false;
    if (polygon.size() < 3) {
        return false;
    }
    for (std::size_t i = 0, j = polygon.size() - 1; i < polygon.size(); j = i++) {
        const Vec2 a = polygon[j];
        const Vec2 b = polygon[i];
        const bool straddles = (a.y > point.y) != (b.y > point.y);
        if (straddles) {
            const double x = a.x + (point.y - a.y) * (b.x - a.x) / (b.y - a.y);
            if (x > point.x) {
                inside = !inside;
            }
        }
    }
    return inside;
}

}  // namespace fill
