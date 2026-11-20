#include "core/Geometry.h"

#include <algorithm>
#include <cstdio>
#include <cstdlib>
#include <locale>
#include <sstream>

namespace meshedit {

Vec3 closestPointOnTriangle(const Vec3& p, const Vec3& a, const Vec3& b, const Vec3& c) {
    const Vec3 ab = b - a;
    const Vec3 ac = c - a;
    const Vec3 ap = p - a;
    const double d1 = dot(ab, ap);
    const double d2 = dot(ac, ap);
    if (d1 <= 0.0 && d2 <= 0.0) return a;

    const Vec3 bp = p - b;
    const double d3 = dot(ab, bp);
    const double d4 = dot(ac, bp);
    if (d3 >= 0.0 && d4 <= d3) return b;

    const double vc = d1 * d4 - d3 * d2;
    if (vc <= 0.0 && d1 >= 0.0 && d3 <= 0.0) {
        const double v = d1 / (d1 - d3);
        return a + ab * v;
    }

    const Vec3 cp = p - c;
    const double d5 = dot(ab, cp);
    const double d6 = dot(ac, cp);
    if (d6 >= 0.0 && d5 <= d6) return c;

    const double vb = d5 * d2 - d1 * d6;
    if (vb <= 0.0 && d2 >= 0.0 && d6 <= 0.0) {
        const double w = d2 / (d2 - d6);
        return a + ac * w;
    }

    const double va = d3 * d6 - d5 * d4;
    if (va <= 0.0 && (d4 - d3) >= 0.0 && (d5 - d6) >= 0.0) {
        const double w = (d4 - d3) / ((d4 - d3) + (d5 - d6));
        return b + (c - b) * w;
    }

    const double denom = 1.0 / (va + vb + vc);
    const double v = vb * denom;
    const double w = vc * denom;
    return a + ab * v + ac * w;
}

double pointAabbDistanceSquared(const Vec3& p, const Aabb& box) {
    if (!box.valid()) return std::numeric_limits<double>::infinity();
    double result = 0.0;
    for (std::size_t i = 0; i < 3; ++i) {
        const double value = p[i];
        if (value < box.min[i]) {
            const double d = box.min[i] - value;
            result += d * d;
        } else if (value > box.max[i]) {
            const double d = value - box.max[i];
            result += d * d;
        }
    }
    return result;
}

RayTriangleHit rayTriangle(const Vec3& origin, const Vec3& direction,
                           const Vec3& a, const Vec3& b, const Vec3& c) {
    constexpr double epsilon = 1e-12;
    const Vec3 e1 = b - a;
    const Vec3 e2 = c - a;
    const Vec3 p = cross(direction, e2);
    const double det = dot(e1, p);
    if (std::fabs(det) < epsilon) return {};
    const double invDet = 1.0 / det;
    const Vec3 tvec = origin - a;
    const double u = dot(tvec, p) * invDet;
    if (u < 0.0 || u > 1.0) return {};
    const Vec3 q = cross(tvec, e1);
    const double v = dot(direction, q) * invDet;
    if (v < 0.0 || u + v > 1.0) return {};
    const double t = dot(e2, q) * invDet;
    if (t <= epsilon) return {};
    return {true, t};
}

std::string formatDouble(double value, int precision) {
    if (value == 0.0) value = 0.0;
    std::ostringstream stream;
    stream.imbue(std::locale::classic());
    stream.setf(std::ios::fmtflags(0), std::ios::floatfield);
    stream.precision(precision);
    stream << value;
    return stream.str();
}

} // namespace meshedit
