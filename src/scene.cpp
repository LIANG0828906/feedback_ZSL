#include "scene.hpp"

#include "sha256.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <utility>
#include <vector>

namespace mosaic {

namespace {

constexpr std::uint32_t kMaxDimension = 8192;
constexpr std::uint64_t kMaxPixels = 64ULL * 1024ULL * 1024ULL;
constexpr std::uint64_t kMaxSceneSeed = 9007199254740991ULL;
constexpr std::uint32_t kMaxId = 2147483647U;
constexpr double kDegenerateCrossSquared = 1.0e-24;

class Validator {
public:
    void add(std::string code, std::string path, std::string message) {
        errors_.push_back({std::move(code), std::move(path), std::move(message)});
    }

    std::vector<ValidationError> take() { return std::move(errors_); }

private:
    std::vector<ValidationError> errors_;
};

const JsonValue* requiredField(
    const JsonValue& object,
    std::string_view key,
    const std::string& path,
    Validator& validator) {
    if (!object.isObject()) {
        validator.add(
            "MOSAIC_FIELD_TYPE",
            path,
            "expected object while reading field '" + std::string(key) + "'");
        return nullptr;
    }
    const JsonValue* value = object.find(key);
    if (value == nullptr) {
        validator.add(
            "MOSAIC_FIELD_MISSING",
            path + "." + std::string(key),
            "required field is missing");
    }
    return value;
}

std::optional<double> readNumber(
    const JsonValue& object,
    std::string_view key,
    const std::string& path,
    Validator& validator) {
    const JsonValue* value = requiredField(object, key, path, validator);
    if (value == nullptr) {
        return std::nullopt;
    }
    if (!value->isNumber()) {
        validator.add(
            "MOSAIC_FIELD_TYPE",
            path + "." + std::string(key),
            "expected number");
        return std::nullopt;
    }
    return value->asNumber();
}

std::optional<std::uint64_t> readInteger(
    const JsonValue& object,
    std::string_view key,
    const std::string& path,
    Validator& validator,
    std::uint64_t minimum,
    std::uint64_t maximum) {
    const auto number = readNumber(object, key, path, validator);
    if (!number.has_value()) {
        return std::nullopt;
    }
    if (*number < 0.0 || std::trunc(*number) != *number || *number > 9007199254740991.0) {
        validator.add(
            "MOSAIC_INTEGER_INVALID",
            path + "." + std::string(key),
            "expected a non-negative integer");
        return std::nullopt;
    }
    const auto integer = static_cast<std::uint64_t>(*number);
    if (integer < minimum || integer > maximum) {
        validator.add(
            "MOSAIC_INTEGER_OUT_OF_RANGE",
            path + "." + std::string(key),
            "integer outside supported range");
        return std::nullopt;
    }
    return integer;
}

std::optional<Vec3> readVec3(
    const JsonValue& object,
    std::string_view key,
    const std::string& path,
    Validator& validator) {
    const JsonValue* value = requiredField(object, key, path, validator);
    const std::string valuePath = path + "." + std::string(key);
    if (value == nullptr) {
        return std::nullopt;
    }
    if (!value->isArray() || value->size() != 3) {
        validator.add("MOSAIC_VECTOR_INVALID", valuePath, "expected an array of 3 numbers");
        return std::nullopt;
    }
    Vec3 result;
    double* components[3] = {&result.x, &result.y, &result.z};
    for (std::size_t i = 0; i < 3; ++i) {
        const JsonValue& component = value->at(i);
        if (!component.isNumber() || !std::isfinite(component.asNumber())) {
            validator.add(
                "MOSAIC_VECTOR_INVALID",
                valuePath + "[" + std::to_string(i) + "]",
                "expected a finite number");
            return std::nullopt;
        }
        *components[i] = component.asNumber();
    }
    return result;
}

std::optional<Rgb> readRgb(
    const JsonValue& object,
    std::string_view key,
    const std::string& path,
    Validator& validator) {
    const JsonValue* value = requiredField(object, key, path, validator);
    const std::string valuePath = path + "." + std::string(key);
    if (value == nullptr) {
        return std::nullopt;
    }
    if (!value->isArray() || value->size() != 3) {
        validator.add("MOSAIC_COLOR_INVALID", valuePath, "expected an array of 3 numbers");
        return std::nullopt;
    }
    Rgb result;
    double* components[3] = {&result.r, &result.g, &result.b};
    for (std::size_t i = 0; i < 3; ++i) {
        const JsonValue& component = value->at(i);
        if (!component.isNumber() || !std::isfinite(component.asNumber())) {
            validator.add(
                "MOSAIC_COLOR_INVALID",
                valuePath + "[" + std::to_string(i) + "]",
                "expected a finite number");
            return std::nullopt;
        }
        *components[i] = component.asNumber();
    }
    return result;
}

bool meshIndexValid(const Mesh& mesh, const Triangle& triangle) {
    for (const std::uint32_t index : triangle.vertices) {
        if (index >= mesh.vertices.size()) {
            return false;
        }
    }
    return true;
}

std::optional<Mesh> readMesh(
    const JsonValue& object,
    const std::string& path,
    Validator& validator) {
    const JsonValue* verticesValue = requiredField(object, "vertices", path, validator);
    const JsonValue* trianglesValue = requiredField(object, "triangles", path, validator);
    if (verticesValue == nullptr || trianglesValue == nullptr) {
        return std::nullopt;
    }

    Mesh mesh;
    const std::string verticesPath = path + ".vertices";
    if (!verticesValue->isArray()) {
        validator.add("MOSAIC_MESH_INVALID", verticesPath, "expected an array");
        return std::nullopt;
    }
    if (verticesValue->size() < 3) {
        validator.add("MOSAIC_MESH_INVALID", verticesPath, "at least 3 vertices are required");
    }
    mesh.vertices.reserve(verticesValue->size());
    for (std::size_t i = 0; i < verticesValue->size(); ++i) {
        const JsonValue& vertex = verticesValue->at(i);
        const std::string vertexPath = verticesPath + "[" + std::to_string(i) + "]";
        if (!vertex.isArray() || vertex.size() != 3) {
            validator.add("MOSAIC_VECTOR_INVALID", vertexPath, "expected an array of 3 numbers");
            continue;
        }
        Vec3 point;
        double* components[3] = {&point.x, &point.y, &point.z};
        bool valid = true;
        for (std::size_t axis = 0; axis < 3; ++axis) {
            const JsonValue& component = vertex.at(axis);
            if (!component.isNumber() || !std::isfinite(component.asNumber())) {
                validator.add(
                    "MOSAIC_VECTOR_INVALID",
                    vertexPath + "[" + std::to_string(axis) + "]",
                    "expected a finite number");
                valid = false;
                break;
            }
            *components[axis] = component.asNumber();
        }
        if (valid) {
            mesh.vertices.push_back(point);
        }
    }

    const std::string trianglesPath = path + ".triangles";
    if (!trianglesValue->isArray()) {
        validator.add("MOSAIC_MESH_INVALID", trianglesPath, "expected an array");
        return std::nullopt;
    }
    if (trianglesValue->size() == 0) {
        validator.add("MOSAIC_MESH_INVALID", trianglesPath, "at least 1 triangle is required");
    }

    std::vector<Triangle> validTriangles;
    for (std::size_t i = 0; i < trianglesValue->size(); ++i) {
        const JsonValue& triangleValue = trianglesValue->at(i);
        const std::string trianglePath = trianglesPath + "[" + std::to_string(i) + "]";
        if (!triangleValue.isArray() || triangleValue.size() != 3) {
            validator.add(
                "MOSAIC_MESH_INVALID",
                trianglePath,
                "expected an array of 3 vertex indices");
            continue;
        }
        Triangle triangle;
        bool valid = true;
        for (std::size_t axis = 0; axis < 3; ++axis) {
            const JsonValue& indexValue = triangleValue.at(axis);
            const std::string indexPath = trianglePath + "[" + std::to_string(axis) + "]";
            if (!indexValue.isNumber() || indexValue.asNumber() < 0.0 ||
                std::trunc(indexValue.asNumber()) != indexValue.asNumber() ||
                indexValue.asNumber() >
                    static_cast<double>(std::numeric_limits<std::uint32_t>::max())) {
                validator.add(
                    "MOSAIC_MESH_INDEX_OUT_OF_RANGE",
                    indexPath,
                    "vertex index must be a non-negative 32-bit integer");
                valid = false;
                break;
            }
            triangle.vertices[axis] = static_cast<std::uint32_t>(indexValue.asNumber());
        }
        if (!valid) {
            continue;
        }
        if (!meshIndexValid(mesh, triangle)) {
            for (std::size_t axis = 0; axis < 3; ++axis) {
                if (triangle.vertices[axis] >= mesh.vertices.size()) {
                    validator.add(
                        "MOSAIC_MESH_INDEX_OUT_OF_RANGE",
                        trianglePath + "[" + std::to_string(axis) + "]",
                        "vertex index is outside scene.pieces[].vertices");
                }
            }
            continue;
        }
        const Vec3& a = mesh.vertices[triangle.vertices[0]];
        const Vec3& b = mesh.vertices[triangle.vertices[1]];
        const Vec3& c = mesh.vertices[triangle.vertices[2]];
        if (triangleAreaSquaredTimesFour(a, b, c) <= kDegenerateCrossSquared) {
            validator.add(
                "MOSAIC_TRIANGLE_DEGENERATE",
                trianglePath,
                "triangle has zero or near-zero area");
            continue;
        }
        validTriangles.push_back(triangle);
    }
    mesh.triangles = std::move(validTriangles);

    std::sort(mesh.triangles.begin(), mesh.triangles.end(), [](const Triangle& left, const Triangle& right) {
        auto leftSorted = left.vertices;
        auto rightSorted = right.vertices;
        std::sort(leftSorted.begin(), leftSorted.end());
        std::sort(rightSorted.begin(), rightSorted.end());
        return leftSorted < rightSorted;
    });
    return mesh;
}

double pointTriangleDistanceSquared(const Vec3& point, const Vec3& a, const Vec3& b, const Vec3& c) {
    const Vec3 ab = b - a;
    const Vec3 ac = c - a;
    const Vec3 ap = point - a;
    const double d1 = dot(ab, ap);
    const double d2 = dot(ac, ap);
    if (d1 <= 0.0 && d2 <= 0.0) {
        return lengthSquared(ap);
    }

    const Vec3 bp = point - b;
    const double d3 = dot(ab, bp);
    const double d4 = dot(ac, bp);
    if (d3 >= 0.0 && d4 <= d3) {
        return lengthSquared(bp);
    }

    const double vc = d1 * d4 - d3 * d2;
    if (vc <= 0.0 && d1 >= 0.0 && d3 <= 0.0) {
        const double v = d1 / (d1 - d3);
        return lengthSquared(point - (a + ab * v));
    }

    const Vec3 cp = point - c;
    const double d5 = dot(ab, cp);
    const double d6 = dot(ac, cp);
    if (d6 >= 0.0 && d5 <= d6) {
        return lengthSquared(cp);
    }

    const double vb = d5 * d2 - d1 * d6;
    if (vb <= 0.0 && d2 >= 0.0 && d6 <= 0.0) {
        const double w = d2 / (d2 - d6);
        return lengthSquared(point - (a + ac * w));
    }

    const double va = d3 * d6 - d5 * d4;
    if (va <= 0.0 && (d4 - d3) >= 0.0 && (d5 - d6) >= 0.0) {
        const double w = (d4 - d3) / ((d4 - d3) + (d5 - d6));
        return lengthSquared(point - (b + (c - b) * w));
    }

    const double denominator = va + vb + vc;
    if (std::abs(denominator) <= kEpsilon) {
        return std::min({
            lengthSquared(point - a),
            lengthSquared(point - b),
            lengthSquared(point - c),
        });
    }
    const double v = vb / denominator;
    const double w = vc / denominator;
    const Vec3 closest = a + ab * v + ac * w;
    return lengthSquared(point - closest);
}

bool rayTriangleIntersection(
    const Vec3& origin,
    const Vec3& direction,
    const Vec3& a,
    const Vec3& b,
    const Vec3& c,
    double& distance) {
    const Vec3 edge1 = b - a;
    const Vec3 edge2 = c - a;
    const Vec3 h = cross(direction, edge2);
    const double determinant = dot(edge1, h);
    if (std::abs(determinant) <= 1.0e-12) {
        return false;
    }
    const double inverse = 1.0 / determinant;
    const Vec3 s = origin - a;
    const double u = inverse * dot(s, h);
    if (u < -1.0e-10 || u > 1.0 + 1.0e-10) {
        return false;
    }
    const Vec3 q = cross(s, edge1);
    const double v = inverse * dot(direction, q);
    if (v < -1.0e-10 || u + v > 1.0 + 1.0e-10) {
        return false;
    }
    distance = inverse * dot(edge2, q);
    return distance > 1.0e-8;
}

bool pointOnMesh(const Mesh& mesh, const Vec3& point) {
    constexpr double toleranceSquared = 1.0e-16;
    for (const Triangle& triangle : mesh.triangles) {
        if (pointTriangleDistanceSquared(
                point,
                mesh.vertices[triangle.vertices[0]],
                mesh.vertices[triangle.vertices[1]],
                mesh.vertices[triangle.vertices[2]]) <= toleranceSquared) {
            return true;
        }
    }
    return false;
}

void appendBits(std::string& out, std::string_view name, double value) {
    out += name;
    out += '=';
    out += hex64(doubleBits(value));
    out += ';';
}

void appendInt(std::string& out, std::string_view name, std::uint64_t value) {
    out += name;
    out += '=';
    out += std::to_string(value);
    out += ';';
}

void appendVec(std::string& out, std::string_view name, const Vec3& value) {
    appendBits(out, std::string(name) + ".x", value.x);
    appendBits(out, std::string(name) + ".y", value.y);
    appendBits(out, std::string(name) + ".z", value.z);
}

void appendRgb(std::string& out, std::string_view name, const Rgb& value) {
    appendBits(out, std::string(name) + ".r", value.r);
    appendBits(out, std::string(name) + ".g", value.g);
    appendBits(out, std::string(name) + ".b", value.b);
}

}  // namespace

double triangleAreaSquaredTimesFour(const Vec3& a, const Vec3& b, const Vec3& c) {
    return lengthSquared(cross(b - a, c - a));
}

bool pointInsideMesh(const Mesh& mesh, const Vec3& point) {
    if (mesh.triangles.empty() || pointOnMesh(mesh, point)) {
        return !mesh.triangles.empty();
    }

    constexpr std::array<Vec3, 3> directions = {
        Vec3{1.0, 0.0000013, 0.0000027},
        Vec3{0.0000011, 1.0, 0.0000031},
        Vec3{0.0000033, 0.0000017, 1.0},
    };
    int insideVotes = 0;
    for (const Vec3 direction : directions) {
        int crossings = 0;
        for (const Triangle& triangle : mesh.triangles) {
            double distance = 0.0;
            if (rayTriangleIntersection(
                    point,
                    direction,
                    mesh.vertices[triangle.vertices[0]],
                    mesh.vertices[triangle.vertices[1]],
                    mesh.vertices[triangle.vertices[2]],
                    distance)) {
                ++crossings;
            }
        }
        if ((crossings & 1) != 0) {
            ++insideVotes;
        }
    }
    return insideVotes >= 2;
}

namespace {

std::optional<Camera> parseCamera(const JsonValue& root, Validator& validator) {
    const JsonValue* cameraValue = requiredField(root, "camera", "scene", validator);
    if (cameraValue == nullptr) {
        return std::nullopt;
    }
    if (!cameraValue->isObject()) {
        validator.add("MOSAIC_FIELD_TYPE", "scene.camera", "expected object");
        return std::nullopt;
    }

    Camera camera;
    bool valid = true;
    if (const auto value = readVec3(*cameraValue, "position", "scene.camera", validator)) {
        camera.position = *value;
    } else {
        valid = false;
    }
    if (const auto value = readVec3(*cameraValue, "target", "scene.camera", validator)) {
        camera.target = *value;
    } else {
        valid = false;
    }
    if (const auto value = readVec3(*cameraValue, "up", "scene.camera", validator)) {
        camera.up = *value;
    } else {
        valid = false;
    }
    if (const auto value = readNumber(*cameraValue, "fovYDeg", "scene.camera", validator)) {
        camera.fovYDeg = *value;
    } else {
        valid = false;
    }
    if (const auto value = readNumber(*cameraValue, "near", "scene.camera", validator)) {
        camera.nearPlane = *value;
    } else {
        valid = false;
    }
    if (const auto value = readNumber(*cameraValue, "far", "scene.camera", validator)) {
        camera.farPlane = *value;
    } else {
        valid = false;
    }

    if (!valid) {
        return std::nullopt;
    }
    const Vec3 forward = normalize(camera.target - camera.position);
    if (lengthSquared(camera.target - camera.position) <= 1.0e-18) {
        validator.add(
            "MOSAIC_CAMERA_INVALID",
            "scene.camera.target",
            "camera target must differ from camera position");
        valid = false;
    }
    if (lengthSquared(camera.up) <= 1.0e-18) {
        validator.add("MOSAIC_CAMERA_INVALID", "scene.camera.up", "up vector must be non-zero");
        valid = false;
    } else if (lengthSquared(cross(forward, camera.up)) <= 1.0e-18) {
        validator.add(
            "MOSAIC_CAMERA_INVALID",
            "scene.camera.up",
            "up vector must not be parallel to view direction");
        valid = false;
    }
    if (camera.fovYDeg <= 0.0 || camera.fovYDeg >= 179.0) {
        validator.add(
            "MOSAIC_CAMERA_INVALID",
            "scene.camera.fovYDeg",
            "vertical field of view must be in (0, 179)");
        valid = false;
    }
    if (camera.nearPlane <= 0.0 || camera.farPlane <= camera.nearPlane) {
        validator.add(
            "MOSAIC_CAMERA_INVALID",
            "scene.camera.near",
            "near/far planes must satisfy 0 < near < far");
        valid = false;
    }
    if (!valid) {
        return std::nullopt;
    }
    return camera;
}

std::optional<Settings> parseSettings(const JsonValue& root, Validator& validator) {
    const JsonValue* settingsValue = requiredField(root, "settings", "scene", validator);
    if (settingsValue == nullptr) {
        return std::nullopt;
    }
    if (!settingsValue->isObject()) {
        validator.add("MOSAIC_FIELD_TYPE", "scene.settings", "expected object");
        return std::nullopt;
    }

    Settings settings;
    bool valid = true;

    const JsonValue* imageValue = requiredField(*settingsValue, "image", "scene.settings", validator);
    if (imageValue == nullptr || !imageValue->isObject()) {
        if (imageValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.settings.image", "expected object");
        }
        valid = false;
    } else {
        auto readDimension = [&](std::string_view key, const std::string& path) -> std::optional<std::uint32_t> {
            const auto value = readInteger(*imageValue, key, path, validator, 1, kMaxDimension);
            if (!value.has_value()) {
                return std::nullopt;
            }
            return static_cast<std::uint32_t>(*value);
        };
        if (const auto value = readDimension("width", "scene.settings.image")) {
            settings.output.width = *value;
        } else {
            valid = false;
        }
        if (const auto value = readDimension("height", "scene.settings.image")) {
            settings.output.height = *value;
        } else {
            valid = false;
        }
        if (const auto value = readDimension("chunkWidth", "scene.settings.image")) {
            settings.output.chunkWidth = *value;
        } else {
            valid = false;
        }
        if (const auto value = readDimension("chunkHeight", "scene.settings.image")) {
            settings.output.chunkHeight = *value;
        } else {
            valid = false;
        }
        if (settings.output.width != 0 && settings.output.height != 0 &&
            static_cast<std::uint64_t>(settings.output.width) * settings.output.height > kMaxPixels) {
            validator.add(
                "MOSAIC_IMAGE_TOO_LARGE",
                "scene.settings.image",
                "image exceeds 64 megapixels");
            valid = false;
        }
    }

    const JsonValue* textureValue =
        requiredField(*settingsValue, "texture", "scene.settings", validator);
    if (textureValue == nullptr || !textureValue->isObject()) {
        if (textureValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.settings.texture", "expected object");
        }
        valid = false;
    } else {
        if (const auto value = readInteger(
                *textureValue, "bubblesPerPiece", "scene.settings.texture", validator, 0, 64)) {
            settings.texture.bubblesPerPiece = static_cast<std::uint32_t>(*value);
        } else {
            valid = false;
        }
        if (const auto value = readInteger(
                *textureValue, "flowBandsPerPiece", "scene.settings.texture", validator, 0, 32)) {
            settings.texture.flowBandsPerPiece = static_cast<std::uint32_t>(*value);
        } else {
            valid = false;
        }
    }

    const JsonValue* causticValue =
        requiredField(*settingsValue, "caustic", "scene.settings", validator);
    if (causticValue == nullptr || !causticValue->isObject()) {
        if (causticValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.settings.caustic", "expected object");
        }
        valid = false;
    } else {
        if (const auto value = readInteger(
                *causticValue, "samplesPerEdge", "scene.settings.caustic", validator, 1, 256)) {
            settings.caustic.samplesPerEdge = static_cast<std::uint32_t>(*value);
        } else {
            valid = false;
        }
        auto readCausticDouble = [&](std::string_view key, double minimum, double maximum) -> std::optional<double> {
            const auto value = readNumber(*causticValue, key, "scene.settings.caustic", validator);
            if (!value.has_value()) {
                return std::nullopt;
            }
            if (*value < minimum || *value > maximum) {
                validator.add(
                    "MOSAIC_CAUSTIC_SETTING_OUT_OF_RANGE",
                    "scene.settings.caustic." + std::string(key),
                    "value outside supported range");
                return std::nullopt;
            }
            return value;
        };
        if (const auto value = readCausticDouble("penumbraBase", 0.0, 1.0)) {
            settings.caustic.penumbraBase = *value;
        } else {
            valid = false;
        }
        if (const auto value = readCausticDouble("penumbraThickness", 0.0, 2.0)) {
            settings.caustic.penumbraThickness = *value;
        } else {
            valid = false;
        }
        if (const auto value = readCausticDouble("penumbraLayerDistance", 0.0, 2.0)) {
            settings.caustic.penumbraLayerDistance = *value;
        } else {
            valid = false;
        }
        if (const auto value = readCausticDouble("intensity", 0.000001, 10.0)) {
            settings.caustic.intensity = *value;
        } else {
            valid = false;
        }
        if (const auto value = readCausticDouble("edgeBand", 0.000001, 0.5)) {
            settings.caustic.edgeBand = *value;
        } else {
            valid = false;
        }
        if (const auto value = readCausticDouble("splatSupport", 1.0, 8.0)) {
            settings.caustic.splatSupport = *value;
        } else {
            valid = false;
        }
    }

    const JsonValue* glareValue =
        requiredField(*settingsValue, "glare", "scene.settings", validator);
    if (glareValue == nullptr || !glareValue->isObject()) {
        if (glareValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.settings.glare", "expected object");
        }
        valid = false;
    } else {
        const auto reflectionScale =
            readNumber(*glareValue, "reflectionScale", "scene.settings.glare", validator);
        if (reflectionScale.has_value() && *reflectionScale >= 0.0 && *reflectionScale <= 2.0) {
            settings.glare.reflectionScale = *reflectionScale;
        } else {
            if (reflectionScale.has_value()) {
                validator.add(
                    "MOSAIC_GLARE_SETTING_OUT_OF_RANGE",
                    "scene.settings.glare.reflectionScale",
                    "value must be in [0, 2]");
            }
            valid = false;
        }
        const auto edgePower =
            readNumber(*glareValue, "edgePower", "scene.settings.glare", validator);
        if (edgePower.has_value() && *edgePower >= 1.0 && *edgePower <= 32.0) {
            settings.glare.edgePower = *edgePower;
        } else {
            if (edgePower.has_value()) {
                validator.add(
                    "MOSAIC_GLARE_SETTING_OUT_OF_RANGE",
                    "scene.settings.glare.edgePower",
                    "value must be in [1, 32]");
            }
            valid = false;
        }
    }

    const JsonValue* orderValue =
        requiredField(*settingsValue, "compositionOrder", "scene.settings", validator);
    if (orderValue == nullptr || !orderValue->isArray() || orderValue->size() != 2) {
        if (orderValue != nullptr) {
            validator.add(
                "MOSAIC_COMPOSITION_ORDER_INVALID",
                "scene.settings.compositionOrder",
                "expected exactly two entries");
        }
        valid = false;
    } else {
        bool seenTransmission = false;
        bool seenReflection = false;
        bool orderValid = true;
        for (std::size_t i = 0; i < 2; ++i) {
            const JsonValue& entry = orderValue->at(i);
            const std::string entryPath =
                "scene.settings.compositionOrder[" + std::to_string(i) + "]";
            if (!entry.isString()) {
                validator.add("MOSAIC_COMPOSITION_ORDER_INVALID", entryPath, "expected string");
                orderValid = false;
                continue;
            }
            settings.compositionOrder[i] = entry.asString();
            if (entry.asString() == "transmission" && !seenTransmission) {
                seenTransmission = true;
            } else if (entry.asString() == "reflection" && !seenReflection) {
                seenReflection = true;
            } else {
                validator.add(
                    "MOSAIC_COMPOSITION_ORDER_INVALID",
                    entryPath,
                    "entries must be a permutation of transmission and reflection");
                orderValid = false;
            }
        }
        if (!seenTransmission || !seenReflection) {
            orderValid = false;
        }
        valid = valid && orderValid;
    }

    if (!valid) {
        return std::nullopt;
    }
    return settings;
}

}  // namespace

SceneParseResult parseScene(std::string_view text) {
    SceneParseResult result;
    JsonParseResult parsed = parseJson(text);
    if (parsed.error.has_value()) {
        result.jsonError = parsed.error;
        return result;
    }
    const JsonValue& root = *parsed.value;
    if (!root.isObject()) {
        result.errors.push_back({"MOSAIC_SCENE_INVALID", "scene", "root value must be an object"});
        return result;
    }

    Validator validator;
    const JsonValue* format = requiredField(root, "format", "scene", validator);
    if (format != nullptr) {
        if (!format->isString() || format->asString() != "mosaic-scene-v1") {
            validator.add(
                "MOSAIC_FORMAT_UNSUPPORTED",
                "scene.format",
                "supported format is mosaic-scene-v1");
        }
    }

    Scene scene;
    bool rootValid = true;
    if (const auto seed =
            readInteger(root, "seed", "scene", validator, 0, kMaxSceneSeed)) {
        scene.seed = *seed;
    } else {
        rootValid = false;
    }
    if (const auto camera = parseCamera(root, validator)) {
        scene.camera = *camera;
    } else {
        rootValid = false;
    }
    if (const auto settings = parseSettings(root, validator)) {
        scene.settings = *settings;
    } else {
        rootValid = false;
    }

    const JsonValue* floorValue = requiredField(root, "floor", "scene", validator);
    if (floorValue == nullptr || !floorValue->isObject()) {
        if (floorValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.floor", "expected object");
        }
        rootValid = false;
    } else if (const auto mesh = readMesh(*floorValue, "scene.floor", validator)) {
        scene.floor = *mesh;
    } else {
        rootValid = false;
    }

    const JsonValue* piecesValue = requiredField(root, "pieces", "scene", validator);
    if (piecesValue == nullptr || !piecesValue->isArray()) {
        if (piecesValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.pieces", "expected array");
        }
        rootValid = false;
    } else {
        if (piecesValue->size() == 0) {
            validator.add("MOSAIC_FIELD_MISSING", "scene.pieces", "at least one piece is required");
            rootValid = false;
        }
        std::set<std::pair<std::int32_t, std::uint32_t>> pieceKeys;
        std::set<std::int32_t> layerIndices;
        for (std::size_t i = 0; i < piecesValue->size(); ++i) {
            const JsonValue& pieceValue = piecesValue->at(i);
            const std::string piecePath = "scene.pieces[" + std::to_string(i) + "]";
            if (!pieceValue.isObject()) {
                validator.add("MOSAIC_FIELD_TYPE", piecePath, "expected object");
                rootValid = false;
                continue;
            }
            Piece piece;
            bool pieceValid = true;

            const auto layerValue =
                readInteger(pieceValue, "layerIndex", piecePath, validator, 0, kMaxId);
            const auto pieceIdValue =
                readInteger(pieceValue, "pieceId", piecePath, validator, 0, kMaxId);
            if (layerValue.has_value()) {
                piece.layerIndex = static_cast<std::int32_t>(*layerValue);
            } else {
                pieceValid = false;
            }
            if (pieceIdValue.has_value()) {
                piece.pieceId = static_cast<std::uint32_t>(*pieceIdValue);
            } else {
                pieceValid = false;
            }
            if (layerValue.has_value() && pieceIdValue.has_value()) {
                const auto key = std::make_pair(piece.layerIndex, piece.pieceId);
                if (!pieceKeys.insert(key).second) {
                    validator.add(
                        "MOSAIC_PIECE_KEY_DUPLICATE",
                        piecePath,
                        "duplicate layerIndex/pieceId pair");
                    pieceValid = false;
                }
                layerIndices.insert(piece.layerIndex);
            }

            if (const auto value =
                    readRgb(pieceValue, "transmission", piecePath, validator)) {
                piece.transmission = *value;
                if (piece.transmission.r < 0.0 || piece.transmission.r > 1.0 ||
                    piece.transmission.g < 0.0 || piece.transmission.g > 1.0 ||
                    piece.transmission.b < 0.0 || piece.transmission.b > 1.0) {
                    validator.add(
                        "MOSAIC_TRANSMISSION_OUT_OF_RANGE",
                        piecePath + ".transmission",
                        "transmission components must be in [0, 1]");
                    pieceValid = false;
                }
            } else {
                pieceValid = false;
            }

            if (const auto value = readNumber(pieceValue, "thickness", piecePath, validator)) {
                piece.thickness = *value;
                if (!(piece.thickness > 0.0) || piece.thickness > 1.0) {
                    validator.add(
                        "MOSAIC_THICKNESS_OUT_OF_RANGE",
                        piecePath + ".thickness",
                        "thickness must be in (0, 1]");
                    pieceValid = false;
                }
            } else {
                pieceValid = false;
            }

            if (const auto value = readNumber(pieceValue, "edgeIor", piecePath, validator)) {
                piece.edgeIor = *value;
                if (piece.edgeIor < 1.0 || piece.edgeIor > 3.0) {
                    validator.add(
                        "MOSAIC_IOR_OUT_OF_RANGE",
                        piecePath + ".edgeIor",
                        "edge refractive index must be in [1, 3]");
                    pieceValid = false;
                }
            } else {
                pieceValid = false;
            }

            if (const auto mesh = readMesh(pieceValue, piecePath, validator)) {
                piece.mesh = *mesh;
            } else {
                pieceValid = false;
            }
            if (pieceValid) {
                scene.pieces.push_back(std::move(piece));
            } else {
                rootValid = false;
            }
        }
        (void)layerIndices;
    }

    std::sort(scene.pieces.begin(), scene.pieces.end(), [](const Piece& left, const Piece& right) {
        if (left.layerIndex != right.layerIndex) {
            return left.layerIndex < right.layerIndex;
        }
        return left.pieceId < right.pieceId;
    });

    const JsonValue* lightsValue = requiredField(root, "lights", "scene", validator);
    if (lightsValue == nullptr || !lightsValue->isArray()) {
        if (lightsValue != nullptr) {
            validator.add("MOSAIC_FIELD_TYPE", "scene.lights", "expected array");
        }
        rootValid = false;
    } else {
        if (lightsValue->size() == 0) {
            validator.add("MOSAIC_FIELD_MISSING", "scene.lights", "at least one light is required");
            rootValid = false;
        }
        std::set<std::uint32_t> lightIds;
        for (std::size_t i = 0; i < lightsValue->size(); ++i) {
            const JsonValue& lightValue = lightsValue->at(i);
            const std::string lightPath = "scene.lights[" + std::to_string(i) + "]";
            if (!lightValue.isObject()) {
                validator.add("MOSAIC_FIELD_TYPE", lightPath, "expected object");
                rootValid = false;
                continue;
            }
            Light light;
            bool lightValid = true;
            bool positionValid = false;
            if (const auto value =
                    readInteger(lightValue, "id", lightPath, validator, 0, kMaxId)) {
                light.id = static_cast<std::uint32_t>(*value);
                if (!lightIds.insert(light.id).second) {
                    validator.add(
                        "MOSAIC_LIGHT_ID_DUPLICATE",
                        lightPath + ".id",
                        "light id must be unique");
                    lightValid = false;
                }
            } else {
                lightValid = false;
            }
            if (const auto value =
                    readVec3(lightValue, "position", lightPath, validator)) {
                light.position = *value;
                positionValid = true;
            } else {
                lightValid = false;
            }
            if (const auto value = readRgb(lightValue, "color", lightPath, validator)) {
                light.color = *value;
                if (light.color.r < 0.0 || light.color.r > 1.0 ||
                    light.color.g < 0.0 || light.color.g > 1.0 ||
                    light.color.b < 0.0 || light.color.b > 1.0) {
                    validator.add(
                        "MOSAIC_LIGHT_COLOR_OUT_OF_RANGE",
                        lightPath + ".color",
                        "light color components must be in [0, 1]");
                    lightValid = false;
                }
            } else {
                lightValid = false;
            }
            if (const auto value =
                    readNumber(lightValue, "intensity", lightPath, validator)) {
                light.intensity = *value;
                if (!(light.intensity > 0.0) || light.intensity > 100.0) {
                    validator.add(
                        "MOSAIC_LIGHT_INTENSITY_OUT_OF_RANGE",
                        lightPath + ".intensity",
                        "light intensity must be in (0, 100]");
                    lightValid = false;
                }
            } else {
                lightValid = false;
            }
            if (positionValid && !scene.floor.triangles.empty() &&
                pointInsideMesh(scene.floor, light.position)) {
                validator.add(
                    "MOSAIC_LIGHT_INSIDE_FLOOR",
                    lightPath + ".position",
                    "point light is inside the floor geometry");
                lightValid = false;
            }
            if (lightValid) {
                scene.lights.push_back(light);
            } else {
                rootValid = false;
            }
        }
    }
    std::sort(scene.lights.begin(), scene.lights.end(), [](const Light& left, const Light& right) {
        return left.id < right.id;
    });

    result.errors = validator.take();
    if (!result.errors.empty()) {
        result.scene.reset();
        return result;
    }
    if (!rootValid) {
        result.errors.push_back(
            {"MOSAIC_SCENE_INVALID", "scene", "scene could not be fully validated"});
        result.scene.reset();
        return result;
    }
    result.scene = std::move(scene);
    return result;
}

std::string sceneDigest(const Scene& scene) {
    std::string canonical;
    canonical.reserve(16384);
    canonical += "mosaic-scene-v1;";
    appendInt(canonical, "seed", scene.seed);
    appendVec(canonical, "camera.position", scene.camera.position);
    appendVec(canonical, "camera.target", scene.camera.target);
    appendVec(canonical, "camera.up", scene.camera.up);
    appendBits(canonical, "camera.fovYDeg", scene.camera.fovYDeg);
    appendBits(canonical, "camera.near", scene.camera.nearPlane);
    appendBits(canonical, "camera.far", scene.camera.farPlane);
    appendInt(canonical, "image.width", scene.settings.output.width);
    appendInt(canonical, "image.height", scene.settings.output.height);
    appendInt(canonical, "image.chunkWidth", scene.settings.output.chunkWidth);
    appendInt(canonical, "image.chunkHeight", scene.settings.output.chunkHeight);
    appendInt(canonical, "texture.bubbles", scene.settings.texture.bubblesPerPiece);
    appendInt(canonical, "texture.flows", scene.settings.texture.flowBandsPerPiece);
    appendInt(canonical, "caustic.samples", scene.settings.caustic.samplesPerEdge);
    appendBits(canonical, "caustic.base", scene.settings.caustic.penumbraBase);
    appendBits(canonical, "caustic.thickness", scene.settings.caustic.penumbraThickness);
    appendBits(canonical, "caustic.distance", scene.settings.caustic.penumbraLayerDistance);
    appendBits(canonical, "caustic.intensity", scene.settings.caustic.intensity);
    appendBits(canonical, "caustic.edgeBand", scene.settings.caustic.edgeBand);
    appendBits(canonical, "caustic.splatSupport", scene.settings.caustic.splatSupport);
    appendBits(canonical, "glare.reflectionScale", scene.settings.glare.reflectionScale);
    appendBits(canonical, "glare.edgePower", scene.settings.glare.edgePower);
    canonical += "order.0=" + scene.settings.compositionOrder[0] + ";";
    canonical += "order.1=" + scene.settings.compositionOrder[1] + ";";

    canonical += "floor.vertices=" + std::to_string(scene.floor.vertices.size()) + ";";
    for (const Vec3& vertex : scene.floor.vertices) {
        appendVec(canonical, "v", vertex);
    }
    canonical += "floor.triangles=" + std::to_string(scene.floor.triangles.size()) + ";";
    for (const Triangle& triangle : scene.floor.triangles) {
        appendInt(canonical, "i0", triangle.vertices[0]);
        appendInt(canonical, "i1", triangle.vertices[1]);
        appendInt(canonical, "i2", triangle.vertices[2]);
    }

    canonical += "pieces=" + std::to_string(scene.pieces.size()) + ";";
    for (const Piece& piece : scene.pieces) {
        appendInt(canonical, "layer", static_cast<std::uint32_t>(piece.layerIndex));
        appendInt(canonical, "piece", piece.pieceId);
        appendRgb(canonical, "transmission", piece.transmission);
        appendBits(canonical, "thickness", piece.thickness);
        appendBits(canonical, "edgeIor", piece.edgeIor);
        appendInt(canonical, "vertices", piece.mesh.vertices.size());
        for (const Vec3& vertex : piece.mesh.vertices) {
            appendVec(canonical, "v", vertex);
        }
        appendInt(canonical, "triangles", piece.mesh.triangles.size());
        for (const Triangle& triangle : piece.mesh.triangles) {
            appendInt(canonical, "i0", triangle.vertices[0]);
            appendInt(canonical, "i1", triangle.vertices[1]);
            appendInt(canonical, "i2", triangle.vertices[2]);
        }
    }

    canonical += "lights=" + std::to_string(scene.lights.size()) + ";";
    for (const Light& light : scene.lights) {
        appendInt(canonical, "id", light.id);
        appendVec(canonical, "position", light.position);
        appendRgb(canonical, "color", light.color);
        appendBits(canonical, "intensity", light.intensity);
    }
    return Sha256::digest(canonical);
}

std::string validationErrorsJson(const std::vector<ValidationError>& errors) {
    std::ostringstream out;
    out << "{\n  \"ok\": false,\n  \"errorCount\": " << errors.size()
        << ",\n  \"errors\": [";
    for (std::size_t i = 0; i < errors.size(); ++i) {
        out << (i == 0 ? "\n" : ",\n");
        out << "    {\"code\": \"" << escapeJsonString(errors[i].code)
            << "\", \"path\": \"" << escapeJsonString(errors[i].path)
            << "\", \"message\": \"" << escapeJsonString(errors[i].message) << "\"}";
    }
    if (!errors.empty()) {
        out << '\n';
    }
    out << "  ]\n}\n";
    return out.str();
}

std::string sceneSummaryJson(const Scene& scene) {
    std::size_t layerCount = 0;
    std::int32_t previousLayer = -1;
    for (const Piece& piece : scene.pieces) {
        if (piece.layerIndex != previousLayer) {
            ++layerCount;
            previousLayer = piece.layerIndex;
        }
    }
    std::ostringstream out;
    out << "{\"digest\": \"" << sceneDigest(scene) << "\", \"seed\": " << scene.seed
        << ", \"width\": " << scene.settings.output.width
        << ", \"height\": " << scene.settings.output.height
        << ", \"chunkWidth\": " << scene.settings.output.chunkWidth
        << ", \"chunkHeight\": " << scene.settings.output.chunkHeight
        << ", \"pieceCount\": " << scene.pieces.size()
        << ", \"layerCount\": " << layerCount
        << ", \"lightCount\": " << scene.lights.size() << "}";
    return out.str();
}

}  // namespace mosaic
