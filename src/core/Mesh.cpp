#include "core/Mesh.h"

#include <algorithm>
#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <iomanip>
#include <locale>
#include <sstream>
#include <stdexcept>

namespace meshedit {

MeshMetrics computeMetrics(const Mesh& mesh) {
    MeshMetrics result;
    result.vertexCount = mesh.positions.size();
    result.faceCount = mesh.faces.size();
    for (const Vec3& p : mesh.positions) result.bounds.expand(p);
    double volume = 0.0;
    for (const Face& face : mesh.faces) {
        const int ia = face.corners[0].position;
        const int ib = face.corners[1].position;
        const int ic = face.corners[2].position;
        if (ia < 0 || ib < 0 || ic < 0 ||
            static_cast<std::size_t>(ia) >= mesh.positions.size() ||
            static_cast<std::size_t>(ib) >= mesh.positions.size() ||
            static_cast<std::size_t>(ic) >= mesh.positions.size()) {
            continue;
        }
        const Vec3& a = mesh.positions[static_cast<std::size_t>(ia)];
        const Vec3& b = mesh.positions[static_cast<std::size_t>(ib)];
        const Vec3& c = mesh.positions[static_cast<std::size_t>(ic)];
        volume += dot(a, cross(b, c)) / 6.0;
        result.surfaceArea += 0.5 * length(cross(b - a, c - a));
        result.triangleCornerCount += 3;
    }
    result.volume = std::fabs(volume);
    return result;
}

bool metricsEqual(const MeshMetrics& a, const MeshMetrics& b, double epsilon) {
    auto close = [epsilon](double x, double y) {
        return std::fabs(x - y) <= epsilon + 1e-15 * std::max({1.0, std::fabs(x), std::fabs(y)});
    };
    if (a.vertexCount != b.vertexCount || a.faceCount != b.faceCount ||
        a.triangleCornerCount != b.triangleCornerCount) {
        return false;
    }
    if (a.bounds.valid() != b.bounds.valid()) return false;
    if (a.bounds.valid()) {
        for (std::size_t i = 0; i < 3; ++i) {
            if (!close(a.bounds.min[i], b.bounds.min[i])) return false;
            if (!close(a.bounds.max[i], b.bounds.max[i])) return false;
        }
    }
    return close(a.volume, b.volume) && close(a.surfaceArea, b.surfaceArea);
}

std::string metricsJson(const MeshMetrics& metrics, int indent) {
    const std::string pad(static_cast<std::size_t>(indent), ' ');
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << pad << "{\"vertices\":" << metrics.vertexCount
        << ",\"faces\":" << metrics.faceCount
        << ",\"bounds\":{";
    if (metrics.bounds.valid()) {
        out << "\"min\":[" << formatDouble(metrics.bounds.min.x) << ','
            << formatDouble(metrics.bounds.min.y) << ','
            << formatDouble(metrics.bounds.min.z) << "],\"max\":["
            << formatDouble(metrics.bounds.max.x) << ','
            << formatDouble(metrics.bounds.max.y) << ','
            << formatDouble(metrics.bounds.max.z) << ']';
    } else {
        out << "\"min\":null,\"max\":null";
    }
    out << "},\"volume\":" << formatDouble(metrics.volume)
        << ",\"surfaceArea\":" << formatDouble(metrics.surfaceArea) << '}';
    return out.str();
}

void recomputeNormals(Mesh& mesh) {
    mesh.normals.assign(mesh.positions.size(), Vec3{});
    for (const Face& face : mesh.faces) {
        const int ia = face.corners[0].position;
        const int ib = face.corners[1].position;
        const int ic = face.corners[2].position;
        if (ia < 0 || ib < 0 || ic < 0 ||
            static_cast<std::size_t>(ia) >= mesh.positions.size() ||
            static_cast<std::size_t>(ib) >= mesh.positions.size() ||
            static_cast<std::size_t>(ic) >= mesh.positions.size()) {
            continue;
        }
        const Vec3 normal = cross(mesh.positions[static_cast<std::size_t>(ib)] -
                                      mesh.positions[static_cast<std::size_t>(ia)],
                                  mesh.positions[static_cast<std::size_t>(ic)] -
                                      mesh.positions[static_cast<std::size_t>(ia)]);
        mesh.normals[static_cast<std::size_t>(ia)] += normal;
        mesh.normals[static_cast<std::size_t>(ib)] += normal;
        mesh.normals[static_cast<std::size_t>(ic)] += normal;
    }
    for (Vec3& normal : mesh.normals) normal = normalized(normal);
    for (Face& face : mesh.faces) {
        for (Corner& corner : face.corners) {
            corner.normal = corner.position;
        }
    }
}

void validateMeshIndices(const Mesh& mesh, const char* context) {
    for (std::size_t fi = 0; fi < mesh.faces.size(); ++fi) {
        for (const Corner& corner : mesh.faces[fi].corners) {
            if (corner.position < 0 ||
                static_cast<std::size_t>(corner.position) >= mesh.positions.size()) {
                throw std::runtime_error(std::string(context) + ": bad position index at face " +
                                         std::to_string(fi));
            }
            if (corner.texcoord >= 0 &&
                static_cast<std::size_t>(corner.texcoord) >= mesh.texcoords.size()) {
                throw std::runtime_error(std::string(context) + ": bad texcoord index at face " +
                                         std::to_string(fi));
            }
            if (corner.normal >= 0 &&
                static_cast<std::size_t>(corner.normal) >= mesh.normals.size()) {
                throw std::runtime_error(std::string(context) + ": bad normal index at face " +
                                         std::to_string(fi));
            }
        }
    }
}

std::string trim(const std::string& text) {
    const auto begin = text.find_first_not_of(" \t\r\n");
    if (begin == std::string::npos) return {};
    const auto end = text.find_last_not_of(" \t\r\n");
    return text.substr(begin, end - begin + 1);
}

std::vector<std::string> splitWhitespace(const std::string& text) {
    std::istringstream stream(text);
    stream.imbue(std::locale::classic());
    std::vector<std::string> result;
    std::string token;
    while (stream >> token) result.push_back(token);
    return result;
}

bool parseInteger(const std::string& token, int& value) {
    if (token.empty()) return false;
    errno = 0;
    char* end = nullptr;
    const long parsed = std::strtol(token.c_str(), &end, 10);
    if (errno != 0 || end == token.c_str() || *end != '\0') return false;
    if (parsed < std::numeric_limits<int>::min() || parsed > std::numeric_limits<int>::max()) {
        return false;
    }
    value = static_cast<int>(parsed);
    return true;
}

bool parseDouble(const std::string& token, double& value) {
    if (token.empty()) return false;
    errno = 0;
    char* end = nullptr;
    const double parsed = std::strtod(token.c_str(), &end);
    if (errno != 0 || end == token.c_str() || *end != '\0' || !std::isfinite(parsed)) {
        return false;
    }
    value = parsed;
    return true;
}

bool parseVec2(const std::string& a, const std::string& b, Vec2& value) {
    return parseDouble(a, value.x) && parseDouble(b, value.y);
}

bool parseVec3(const std::string& a, const std::string& b, const std::string& c, Vec3& value) {
    return parseDouble(a, value.x) && parseDouble(b, value.y) && parseDouble(c, value.z);
}

} // namespace meshedit
