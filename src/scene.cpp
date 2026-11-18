#include "scene.hpp"

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <limits>
#include <set>
#include <sstream>
#include <stdexcept>

namespace sl {
namespace {

const sj::Value* key(const sj::Value& object,
                    std::initializer_list<const char*> names) {
    for (const char* name : names) {
        const sj::Value* value = object.find(name);
        if (value != nullptr) return value;
    }
    return nullptr;
}

bool asInt(const sj::Value* value, long long& out) {
    if (value == nullptr || !value->isNumber()) return false;
    double number = value->asNumber();
    if (!std::isfinite(number) || std::floor(number) != number) return false;
    if (number < static_cast<double>(std::numeric_limits<long long>::min()) ||
        number > static_cast<double>(std::numeric_limits<long long>::max())) {
        return false;
    }
    out = static_cast<long long>(number);
    return true;
}

bool asDouble(const sj::Value* value, double& out) {
    if (value == nullptr || !value->isNumber()) return false;
    out = value->asNumber();
    return std::isfinite(out);
}

bool readVec3(const sj::Value* value, Vec3& out) {
    if (value == nullptr || !value->isArray() || value->asArray().size() != 3) return false;
    const auto& values = value->asArray();
    if (!values[0].isNumber() || !values[1].isNumber() || !values[2].isNumber()) return false;
    out = {values[0].asNumber(), values[1].asNumber(), values[2].asNumber()};
    return finite(out);
}

sj::Value vec3Json(const Vec3& value) {
    sj::Value result = sj::Value::array();
    result.push(sj::Value(value.x));
    result.push(sj::Value(value.y));
    result.push(sj::Value(value.z));
    return result;
}

void addError(std::vector<Diagnostic>& diagnostics,
              int layer,
              const std::string& separationId,
              const std::string& path,
              const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.layer = layer;
    diagnostic.separationId = separationId;
    diagnostic.path = path;
    diagnostic.message = message;
    diagnostics.push_back(std::move(diagnostic));
}

void rebuildLayerDerived(ScreenLayer& layer) {
    layer.triangleData.clear();
    layer.boundsMin = {1.0e100, 1.0e100, 1.0e100};
    layer.boundsMax = {-1.0e100, -1.0e100, -1.0e100};
    for (const Vec3& vertex : layer.vertices) {
        layer.boundsMin.x = std::min(layer.boundsMin.x, vertex.x);
        layer.boundsMin.y = std::min(layer.boundsMin.y, vertex.y);
        layer.boundsMin.z = std::min(layer.boundsMin.z, vertex.z);
        layer.boundsMax.x = std::max(layer.boundsMax.x, vertex.x);
        layer.boundsMax.y = std::max(layer.boundsMax.y, vertex.y);
        layer.boundsMax.z = std::max(layer.boundsMax.z, vertex.z);
    }
    for (const auto& indices : layer.triangles) {
        Triangle triangle;
        triangle.a = layer.vertices[static_cast<std::size_t>(indices[0])];
        triangle.b = layer.vertices[static_cast<std::size_t>(indices[1])];
        triangle.c = layer.vertices[static_cast<std::size_t>(indices[2])];
        triangle.finish();
        layer.triangleData.push_back(triangle);
    }
    const Triangle& first = layer.triangleData.front();
    layer.plane.origin = first.a;
    layer.plane.normal = first.normal;
    layer.plane.finish();
    layer.minU = 1.0e100;
    layer.maxU = -1.0e100;
    layer.minV = 1.0e100;
    layer.maxV = -1.0e100;
    for (const Vec3& vertex : layer.vertices) {
        const Vec3 local = vertex - layer.plane.origin;
        const double u = dot(local, layer.plane.u);
        const double v = dot(local, layer.plane.v);
        layer.minU = std::min(layer.minU, u);
        layer.maxU = std::max(layer.maxU, u);
        layer.minV = std::min(layer.minV, v);
        layer.maxV = std::max(layer.maxV, v);
    }
}

void applyDeclaredAirGaps(Scene& scene) {
    if (scene.layers.size() != 4 || scene.airGaps.size() != 3) return;
    const Vec3 baseNormal = scene.layers.front().plane.normal;
    if (length(baseNormal) <= kEpsilon) return;
    for (const ScreenLayer& layer : scene.layers) {
        if (std::fabs(dot(baseNormal, layer.plane.normal)) < 0.98) return;
    }
    const Vec3 baseOrigin = scene.layers.front().plane.origin;
    double cumulativeGap = 0.0;
    for (std::size_t i = 0; i < scene.layers.size(); ++i) {
        if (i > 0) cumulativeGap += scene.airGaps[i - 1];
        ScreenLayer& layer = scene.layers[i];
        const double desiredOffset = cumulativeGap;
        const double currentOffset = dot(layer.plane.origin - baseOrigin, baseNormal);
        const Vec3 shift = baseNormal * (desiredOffset - currentOffset);
        for (Vec3& vertex : layer.vertices) {
            vertex += shift;
        }
        rebuildLayerDerived(layer);
    }
}

bool parseTriangles(const sj::Value* value,
                    std::vector<std::array<int, 3>>& triangles,
                    std::vector<std::string>& messages) {
    if (value == nullptr || !value->isArray()) {
        messages.push_back("triangles/indices must be an array");
        return false;
    }
    const auto& array = value->asArray();
    if (array.empty()) {
        messages.push_back("mesh must contain at least one triangle");
        return false;
    }
    if (array[0].isArray()) {
        for (std::size_t i = 0; i < array.size(); ++i) {
            const auto& item = array[i];
            if (!item.isArray() || item.asArray().size() != 3) {
                messages.push_back("triangle[" + std::to_string(i) + "] must have three indices");
                continue;
            }
            long long indices[3] = {0, 0, 0};
            bool ok = true;
            for (int j = 0; j < 3; ++j) {
                if (!asInt(&item.asArray()[static_cast<std::size_t>(j)], indices[j])) {
                    ok = false;
                } else if (indices[j] < std::numeric_limits<int>::min() ||
                           indices[j] > std::numeric_limits<int>::max()) {
                    ok = false;
                }
            }
            if (!ok) {
                messages.push_back("triangle[" + std::to_string(i) + "] contains a non-integer index");
                continue;
            }
            triangles.push_back({
                static_cast<int>(indices[0]),
                static_cast<int>(indices[1]),
                static_cast<int>(indices[2]),
            });
        }
    } else {
        if (array.size() % 3 != 0) {
            messages.push_back("flat indices array length must be divisible by three");
            return false;
        }
        for (std::size_t i = 0; i < array.size(); i += 3) {
            long long indices[3] = {0, 0, 0};
            bool ok = true;
            for (int j = 0; j < 3; ++j) {
                if (!asInt(&array[i + static_cast<std::size_t>(j)], indices[j]) ||
                    indices[j] < std::numeric_limits<int>::min() ||
                    indices[j] > std::numeric_limits<int>::max()) {
                    ok = false;
                }
            }
            if (!ok) {
                messages.push_back("flat indices contain a non-integer value");
                continue;
            }
            triangles.push_back({
                static_cast<int>(indices[0]),
                static_cast<int>(indices[1]),
                static_cast<int>(indices[2]),
            });
        }
    }
    return !triangles.empty() && messages.empty();
}

bool parseLayer(const sj::Value& object,
                int arrayIndex,
                ScreenLayer& layer,
                std::vector<Diagnostic>& diagnostics) {
    const std::string basePath = "screens[" + std::to_string(arrayIndex) + "]";
    long long integerValue = 0;
    const sj::Value* layerValue = key(object, {"layerIndex", "layer", "index"});
    if (!asInt(layerValue, integerValue) || integerValue <= 0 || integerValue > 1000000) {
        addError(diagnostics, 0, "", basePath + ".layerIndex", "missing or invalid layer index");
    } else {
        layer.layerIndex = static_cast<int>(integerValue);
    }

    const sj::Value* separationValue =
        key(object, {"separationId", "separation", "colorPlate", "plateId",
                     "separationID", "id"});
    if (separationValue == nullptr || !separationValue->isString() ||
        separationValue->asString().empty()) {
        addError(diagnostics, layer.layerIndex, "", basePath + ".separationId",
                 "missing color separation id");
    } else {
        layer.separationId = separationValue->asString();
    }

    const sj::Value* vertices = object.find("vertices");
    if (vertices == nullptr || !vertices->isArray() || vertices->asArray().size() < 3) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".vertices",
                 "vertices must be an array with at least three points");
    } else {
        const auto& array = vertices->asArray();
        for (std::size_t i = 0; i < array.size(); ++i) {
            Vec3 point;
            if (!readVec3(&array[i], point)) {
                addError(diagnostics, layer.layerIndex, layer.separationId,
                         basePath + ".vertices[" + std::to_string(i) + "]",
                         "vertex must contain three finite numbers");
            } else {
                layer.vertices.push_back(point);
            }
        }
    }

    std::vector<std::string> triangleMessages;
    const sj::Value* triangleValue = key(object, {"indices", "triangles", "faces"});
    if (!parseTriangles(triangleValue, layer.triangles, triangleMessages)) {
        for (const std::string& message : triangleMessages) {
            addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".triangles",
                     message);
        }
    }
    for (std::size_t i = 0; i < layer.triangles.size(); ++i) {
        for (int j = 0; j < 3; ++j) {
            const int index = layer.triangles[i][static_cast<std::size_t>(j)];
            if (index < 0 || static_cast<std::size_t>(index) >= layer.vertices.size()) {
                addError(diagnostics, layer.layerIndex, layer.separationId,
                         basePath + ".triangles[" + std::to_string(i) + "]",
                         "mesh index " + std::to_string(index) + " is out of range");
            }
        }
    }

    double number = 0.0;
    const sj::Value* trans = key(object, {"inkTransmittance", "transmittance", "tau"});
    if (!asDouble(trans, number) || number < 0.0 || number > 1.0) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".inkTransmittance",
                 "ink transmittance must be within [0,1]");
    } else {
        layer.inkTransmittance = number;
    }

    const sj::Value* color = key(object, {"color", "inkColor", "separationColor"});
    Vec3 colorValue;
    if (!readVec3(color, colorValue) || colorValue.x < 0.0 || colorValue.y < 0.0 ||
        colorValue.z < 0.0 || colorValue.x > 1.0 || colorValue.y > 1.0 ||
        colorValue.z > 1.0) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".color",
                 "ink color must contain three values within [0,1]");
    } else {
        layer.inkColor = colorValue;
    }

    const sj::Value* mesh = object.find("mesh");
    const sj::Value* line = key(object, {"meshLineCount", "lineCount", "meshLines"});
    if (line == nullptr && mesh != nullptr && mesh->isObject()) {
        line = key(*mesh, {"lineCount", "lines", "frequency"});
    }
    if (!asInt(line, integerValue) || integerValue < 1 || integerValue > 4096) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".mesh.lineCount",
                 "mesh line count must be an integer within [1,4096]");
    } else {
        layer.meshLineCount = static_cast<int>(integerValue);
    }

    const sj::Value* angle = key(object, {"meshAngleDegrees", "angleDegrees", "meshAngle"});
    if (angle == nullptr && mesh != nullptr && mesh->isObject()) {
        angle = key(*mesh, {"angleDegrees", "angle"});
    }
    if (!asDouble(angle, number)) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".mesh.angleDegrees",
                 "mesh angle must be a finite number");
    } else {
        layer.meshAngleDegrees = number;
    }

    const sj::Value* seed = key(object, {"seed", "meshSeed"});
    if (seed == nullptr && mesh != nullptr && mesh->isObject()) {
        seed = key(*mesh, {"seed"});
    }
    if (!asInt(seed, integerValue) || integerValue < 0 ||
        integerValue > 9007199254740991LL) {
        addError(diagnostics, layer.layerIndex, layer.separationId, basePath + ".seed",
                 "mesh seed must be a non-negative integer representable in JSON");
    } else {
        layer.seed = static_cast<std::uint64_t>(integerValue);
    }
    return true;
}

bool closedMesh(const ScreenLayer& layer) {
    std::map<std::pair<int, int>, int> edges;
    for (const auto& tri : layer.triangles) {
        for (int i = 0; i < 3; ++i) {
            int a = tri[static_cast<std::size_t>(i)];
            int b = tri[static_cast<std::size_t>((i + 1) % 3)];
            if (a > b) std::swap(a, b);
            edges[{a, b}]++;
        }
    }
    for (const auto& item : edges) {
        if (item.second < 2) return false;
    }
    return true;
}

bool pointInsideClosedMesh(const ScreenLayer& layer, const Vec3& point) {
    if (!closedMesh(layer)) return false;
    Ray ray{point, {1.0, 0.0, 0.0}};
    int hits = 0;
    for (const Triangle& triangle : layer.triangleData) {
        double t = 0.0;
        double u = 0.0;
        double v = 0.0;
        if (rayTriangle(ray, triangle, t, u, v)) {
            ++hits;
        }
    }
    return (hits % 2) == 1;
}

bool lightOverlapsLayer(const ScreenLayer& layer, const Vec3& point) {
    for (const Triangle& triangle : layer.triangleData) {
        if (pointOnTriangle(point, triangle, 1.0e-6)) return true;
    }
    return pointInsideClosedMesh(layer, point);
}

bool segmentOverlapsLayer(const ScreenLayer& layer, const Vec3& start, const Vec3& end) {
    if (layerContainsPoint(layer, start, 1.0e-6) ||
        layerContainsPoint(layer, end, 1.0e-6) ||
        pointInsideClosedMesh(layer, (start + end) * 0.5)) {
        return true;
    }
    for (const Triangle& triangle : layer.triangleData) {
        const double da = dot(start - triangle.a, triangle.normal);
        const double db = dot(end - triangle.a, triangle.normal);
        if (da * db > 0.0 || std::fabs(da - db) <= 1.0e-12) continue;
        const double t = da / (da - db);
        if (t < 0.0 || t > 1.0) continue;
        const Vec3 point = start + (end - start) * t;
        if (pointOnTriangle(point, triangle, 1.0e-6)) return true;
    }
    return false;
}

sj::Value makeRectangle(double centerX,
                        double centerY,
                        double z,
                        double width,
                        double height) {
    sj::Value vertices = sj::Value::array();
    vertices.push(vec3Json({centerX - width * 0.5, centerY - height * 0.5, z}));
    vertices.push(vec3Json({centerX + width * 0.5, centerY - height * 0.5, z}));
    vertices.push(vec3Json({centerX + width * 0.5, centerY + height * 0.5, z}));
    vertices.push(vec3Json({centerX - width * 0.5, centerY + height * 0.5, z}));
    sj::Value triangleIndices = sj::Value::array();
    triangleIndices.push(sj::Value(0));
    triangleIndices.push(sj::Value(1));
    triangleIndices.push(sj::Value(2));
    triangleIndices.push(sj::Value(0));
    triangleIndices.push(sj::Value(2));
    triangleIndices.push(sj::Value(3));
    sj::Value mesh = sj::Value::object();
    mesh["vertices"] = std::move(vertices);
    mesh["indices"] = std::move(triangleIndices);
    return mesh;
}

sj::Value makeHexagon(double centerX,
                      double centerY,
                      double z,
                      double radiusX,
                      double radiusY) {
    sj::Value vertices = sj::Value::array();
    vertices.push(vec3Json({centerX, centerY, z}));
    for (int i = 0; i < 6; ++i) {
        const double angle = radians(90.0 + static_cast<double>(i) * 60.0);
        vertices.push(vec3Json({
            centerX + std::cos(angle) * radiusX,
            centerY + std::sin(angle) * radiusY,
            z,
        }));
    }
    sj::Value triangleIndices = sj::Value::array();
    for (int i = 0; i < 6; ++i) {
        triangleIndices.push(sj::Value(0));
        triangleIndices.push(sj::Value(1 + i));
        triangleIndices.push(sj::Value(1 + (i + 1) % 6));
    }
    sj::Value mesh = sj::Value::object();
    mesh["vertices"] = std::move(vertices);
    mesh["indices"] = std::move(triangleIndices);
    return mesh;
}

sj::Value makeLayer(const sj::Value& mesh,
                    int layerIndex,
                    const std::string& separation,
                    double transmittance,
                    double r,
                    double g,
                    double b,
                    int lines,
                    double angle,
                    std::uint64_t seed) {
    sj::Value layer = sj::Value::object();
    layer["layerIndex"] = sj::Value(layerIndex);
    layer["separationId"] = sj::Value(separation);
    for (const auto& item : mesh.asObject()) {
        layer[item.first] = item.second;
    }
    layer["inkTransmittance"] = sj::Value(transmittance);
    layer["color"] = vec3Json({r, g, b});
    sj::Value meshParams = sj::Value::object();
    meshParams["lineCount"] = sj::Value(lines);
    meshParams["angleDegrees"] = sj::Value(angle);
    meshParams["seed"] = sj::Value(static_cast<double>(seed));
    layer["mesh"] = std::move(meshParams);
    return layer;
}

} // namespace

double angleDifferenceDegrees(double a, double b) {
    double diff = std::fabs(std::fmod(a - b, 180.0));
    if (diff > 90.0) diff = 180.0 - diff;
    return diff;
}

bool layerContainsPoint(const ScreenLayer& layer, const Vec3& point, double tolerance) {
    for (const Triangle& triangle : layer.triangleData) {
        if (pointOnTriangle(point, triangle, tolerance)) return true;
    }
    return pointInsideClosedMesh(layer, point);
}

double sampleLayerCoverage(const Scene& scene,
                           const ScreenLayer& layer,
                           const Vec3& point,
                           double filterWeight) {
    const double referenceWidth = std::max(scene.backboard.width, 1.0);
    const HalftoneSample sample = layer.halftone.sampleWorld(
        point, layer.plane.origin, layer.plane.u, layer.plane.v,
        referenceWidth, clamp(filterWeight, 0.0, 1.0));
    return sample.coverage(clamp(filterWeight, 0.0, 1.0));
}

bool parseScene(const sj::Value& root, Scene& scene, std::vector<Diagnostic>& diagnostics) {
    diagnostics.clear();
    if (!root.isObject()) {
        addError(diagnostics, 0, "", "$", "scene root must be a JSON object");
        return false;
    }
    scene.sourceCanonical = root.dump();
    scene.sceneDigest = hex64(hashString(scene.sourceCanonical));
    if (const sj::Value* name = root.find("name"); name != nullptr && name->isString()) {
        scene.name = name->asString();
    }
    if (const sj::Value* version = root.find("version"); version != nullptr) {
        long long v = 0;
        if (asInt(version, v) && v > 0) scene.version = static_cast<int>(v);
    }

    const sj::Value* image = root.find("image");
    if (image == nullptr || !image->isObject()) {
        addError(diagnostics, 0, "", "image", "missing image configuration");
    } else {
        long long value = 0;
        if (!asInt(image->find("width"), value) || value < 1 || value > 8192) {
            addError(diagnostics, 0, "", "image.width", "width must be an integer within [1,8192]");
        } else {
            scene.image.width = static_cast<int>(value);
        }
        if (!asInt(image->find("height"), value) || value < 1 || value > 8192) {
            addError(diagnostics, 0, "", "image.height", "height must be an integer within [1,8192]");
        } else {
            scene.image.height = static_cast<int>(value);
        }
        const sj::Value* bw = key(*image, {"blockWidth", "tileWidth"});
        const sj::Value* bh = key(*image, {"blockHeight", "tileHeight"});
        const sj::Value* bs = image->find("blockSize");
        if (bw == nullptr && bs != nullptr) bw = bs;
        if (bh == nullptr && bs != nullptr) bh = bs;
        if (bs != nullptr && bs->isArray() && bs->asArray().size() == 2) {
            bw = &bs->asArray()[0];
            bh = &bs->asArray()[1];
        }
        if (!asInt(bw, value) || value < 1 || value > 4096) {
            addError(diagnostics, 0, "", "image.blockWidth",
                     "block width must be an integer within [1,4096]");
        } else {
            scene.image.blockWidth = static_cast<int>(value);
        }
        if (!asInt(bh, value) || value < 1 || value > 4096) {
            addError(diagnostics, 0, "", "image.blockHeight",
                     "block height must be an integer within [1,4096]");
        } else {
            scene.image.blockHeight = static_cast<int>(value);
        }
        if (const sj::Value* exposure = key(*image, {"exposure", "toneExposure"});
            exposure != nullptr) {
            double e = 0.0;
            if (!asDouble(exposure, e) || e <= 0.0 || e > 100.0) {
                addError(diagnostics, 0, "", "image.exposure",
                         "exposure must be finite and within (0,100]");
            } else {
                scene.image.exposure = e;
            }
        }
    }

    const sj::Value* screens = key(root, {"screens", "layers"});
    if (screens == nullptr || !screens->isArray()) {
        addError(diagnostics, 0, "", "screens", "screens must be an array");
    } else if (screens->asArray().size() != 4) {
        addError(diagnostics, 0, "", "screens",
                 "exactly four screen layers are required, got " +
                     std::to_string(screens->asArray().size()));
    }

    if (screens != nullptr && screens->isArray()) {
        const auto& array = screens->asArray();
        scene.layers.resize(array.size());
        for (std::size_t i = 0; i < array.size(); ++i) {
            if (!array[i].isObject()) {
                addError(diagnostics, 0, "", "screens[" + std::to_string(i) + "]",
                         "layer must be an object");
                continue;
            }
            parseLayer(array[i], static_cast<int>(i), scene.layers[i], diagnostics);
        }
    }

    std::set<int> layerIndices;
    std::set<std::string> separationIds;
    for (const ScreenLayer& layer : scene.layers) {
        if (layer.layerIndex > 0 && !layerIndices.insert(layer.layerIndex).second) {
            addError(diagnostics, layer.layerIndex, layer.separationId, "screens",
                     "duplicate layer index " + std::to_string(layer.layerIndex));
        }
        if (!layer.separationId.empty() && !separationIds.insert(layer.separationId).second) {
            addError(diagnostics, layer.layerIndex, layer.separationId, "screens",
                     "duplicate color separation id " + layer.separationId);
        }
    }
    if (screens != nullptr && screens->isArray() && screens->asArray().size() == 4) {
        for (int expected = 1; expected <= 4; ++expected) {
            if (layerIndices.find(expected) == layerIndices.end()) {
                addError(diagnostics, expected, "", "screens",
                         "missing layer index " + std::to_string(expected));
            }
        }
    }

    for (ScreenLayer& layer : scene.layers) {
        if (layer.vertices.size() < 3 || layer.triangles.empty()) {
            continue;
        }
        bool indicesValid = true;
        for (const auto& tri : layer.triangles) {
            for (int index : tri) {
                if (index < 0 || static_cast<std::size_t>(index) >= layer.vertices.size()) {
                    indicesValid = false;
                }
            }
        }
        if (!indicesValid) {
            continue;
        }
        bool degenerate = false;
        for (std::size_t i = 0; i < layer.triangles.size(); ++i) {
            const auto& tri = layer.triangles[i];
            const Vec3 a = layer.vertices[static_cast<std::size_t>(tri[0])];
            const Vec3 b = layer.vertices[static_cast<std::size_t>(tri[1])];
            const Vec3 c = layer.vertices[static_cast<std::size_t>(tri[2])];
            const double area2 = length(cross(b - a, c - a));
            if (!finite(area2) || area2 <= 1.0e-8) {
                degenerate = true;
                addError(diagnostics, layer.layerIndex, layer.separationId,
                         "screens[" + std::to_string(layer.layerIndex) + "].triangles[" +
                             std::to_string(i) + "]",
                         "degenerate triangle");
            }
        }
        if (degenerate) {
            continue;
        }
        rebuildLayerDerived(layer);
        layer.halftone = HalftoneTable::make(layer.seed, layer.meshLineCount,
                                             layer.meshAngleDegrees);
    }

    const sj::Value* gaps = key(root, {"airGaps", "layerAirGaps", "gaps"});
    if (gaps == nullptr || !gaps->isArray() || gaps->asArray().size() != 3) {
        addError(diagnostics, 0, "", "airGaps",
                 "airGaps must contain exactly three thickness values");
    } else {
        for (std::size_t i = 0; i < 3; ++i) {
            double gap = 0.0;
            if (!asDouble(&gaps->asArray()[i], gap) || gap < 0.0 || gap > 1.0e7) {
                addError(diagnostics, static_cast<int>(i + 1), "", 
                         "airGaps[" + std::to_string(i) + "]",
                         "air gap must be a finite non-negative thickness");
            } else {
                scene.airGaps.push_back(gap);
            }
        }
    }

    const sj::Value* lights = root.find("lights");
    if (lights == nullptr || !lights->isArray() || lights->asArray().size() != 2) {
        addError(diagnostics, 0, "", "lights", "lights must contain exactly two strip lights");
    } else {
        for (std::size_t i = 0; i < 2; ++i) {
            const sj::Value& lightJson = lights->asArray()[i];
            const std::string path = "lights[" + std::to_string(i) + "]";
            if (!lightJson.isObject()) {
                addError(diagnostics, 0, "", path, "light must be an object");
                continue;
            }
            StripLight light;
            Vec3 position;
            Vec3 start;
            Vec3 end;
            const bool hasPosition = readVec3(key(lightJson, {"position", "center"}), position);
            const bool hasSegment = readVec3(lightJson.find("start"), start) &&
                                    readVec3(lightJson.find("end"), end);
            if (!hasPosition && !hasSegment) {
                addError(diagnostics, 0, "", path + ".position",
                         "light requires a finite position or start/end pair");
                continue;
            }
            if (hasSegment) {
                light.a = start;
                light.b = end;
            } else {
                Vec3 direction{1.0, 0.0, 0.0};
                double stripLength = 0.0;
                if (const sj::Value* d = lightJson.find("direction"); d != nullptr) {
                    if (!readVec3(d, direction) || length(direction) <= kEpsilon) {
                        addError(diagnostics, 0, "", path + ".direction",
                                 "light direction must be a non-zero finite vector");
                    }
                }
                if (const sj::Value* l = key(lightJson, {"length", "stripLength"});
                    l != nullptr) {
                    if (!asDouble(l, stripLength) || stripLength < 0.0) {
                        addError(diagnostics, 0, "", path + ".length",
                                 "strip length must be finite and non-negative");
                    }
                }
                if (stripLength > 0.0) {
                    const Vec3 dir = normalize(direction);
                    light.a = position - dir * (stripLength * 0.5);
                    light.b = position + dir * (stripLength * 0.5);
                } else {
                    light.a = position;
                    light.b = position;
                }
            }
            light.midpoint = (light.a + light.b) * 0.5;
            double intensity = 0.0;
            Vec3 color{1.0, 1.0, 1.0};
            const sj::Value* intensityValue = key(lightJson, {"intensity", "strength"});
            if (intensityValue != nullptr && intensityValue->isArray()) {
                Vec3 rgb;
                if (readVec3(intensityValue, rgb) && rgb.x >= 0.0 && rgb.y >= 0.0 &&
                    rgb.z >= 0.0 && rgb.x <= 1.0e6 && rgb.y <= 1.0e6 && rgb.z <= 1.0e6) {
                    color = rgb;
                    intensity = std::max({rgb.x, rgb.y, rgb.z});
                } else {
                    addError(diagnostics, 0, "", path + ".intensity",
                             "RGB intensity must be finite and non-negative");
                }
            } else if (!asDouble(intensityValue, intensity) || intensity < 0.0 ||
                       intensity > 1.0e6) {
                addError(diagnostics, 0, "", path + ".intensity",
                         "light intensity must be finite and non-negative");
            }
            if (const sj::Value* c = key(lightJson, {"color", "tint"}); c != nullptr) {
                Vec3 tint;
                if (!readVec3(c, tint) || tint.x < 0.0 || tint.y < 0.0 || tint.z < 0.0 ||
                    tint.x > 1.0 || tint.y > 1.0 || tint.z > 1.0) {
                    addError(diagnostics, 0, "", path + ".color",
                             "light color must contain three values within [0,1]");
                } else {
                    color = componentMul(color, tint);
                }
            }
            light.color = color;
            light.intensity = intensity;
            scene.lights[i] = light;
        }
        if (scene.lights[0].intensity <= 0.0 && scene.lights[1].intensity <= 0.0) {
            addError(diagnostics, 0, "", "lights", "at least one light must have positive intensity");
        }
    }

    const sj::Value* board = key(root, {"backboard", "backplane", "projectionPlane"});
    if (board == nullptr || !board->isObject()) {
        addError(diagnostics, 0, "", "backboard", "missing backboard plane");
    } else {
        const sj::Value* boardData = board;
        if (const sj::Value* nested = board->find("plane");
            nested != nullptr && nested->isObject()) {
            boardData = nested;
        }
        Vec3 origin;
        Vec3 normal;
        double width = 0.0;
        double height = 0.0;
        if (!readVec3(key(*boardData, {"origin", "point"}), origin)) {
            addError(diagnostics, 0, "", "backboard.origin",
                     "backboard origin must contain three finite values");
        }
        if (!readVec3(key(*boardData, {"normal", "nn"}), normal) ||
            length(normal) <= kEpsilon) {
            addError(diagnostics, 0, "", "backboard.normal",
                     "backboard normal must be a non-zero finite vector");
        }
        const sj::Value* widthValue = key(*board, {"width", "planeWidth"});
        if (widthValue == nullptr) widthValue = key(*boardData, {"width", "planeWidth"});
        const sj::Value* heightValue = key(*board, {"height", "planeHeight"});
        if (heightValue == nullptr) heightValue = key(*boardData, {"height", "planeHeight"});
        if (!asDouble(widthValue, width) || width <= 0.0 ||
            width > 1.0e7) {
            addError(diagnostics, 0, "", "backboard.width",
                     "backboard width must be finite and positive");
        }
        if (!asDouble(heightValue, height) || height <= 0.0 ||
            height > 1.0e7) {
            addError(diagnostics, 0, "", "backboard.height",
                     "backboard height must be finite and positive");
        }
        scene.backboard.origin = origin;
        scene.backboard.normal = normal;
        scene.backboard.width = width;
        scene.backboard.height = height;
        scene.backboard.finish();
        if (const sj::Value* uAxis = key(*boardData, {"uAxis", "axisU"});
            uAxis != nullptr) {
            Vec3 u;
            if (!readVec3(uAxis, u) || length(u) <= kEpsilon) {
                addError(diagnostics, 0, "", "backboard.uAxis",
                         "backboard uAxis must be a non-zero finite vector");
            } else {
                u = u - scene.backboard.normal * dot(u, scene.backboard.normal);
                if (length(u) <= kEpsilon) {
                    addError(diagnostics, 0, "", "backboard.uAxis",
                             "backboard uAxis must not be parallel to its normal");
                } else {
                    scene.backboard.u = normalize(u);
                    scene.backboard.v = normalize(cross(scene.backboard.normal,
                                                        scene.backboard.u));
                }
            }
        }
    }

    const sj::Value* camera = root.find("camera");
    if (camera == nullptr || !camera->isObject()) {
        addError(diagnostics, 0, "", "camera", "missing camera configuration");
    } else {
        Vec3 position;
        Vec3 target;
        Vec3 up{0.0, 1.0, 0.0};
        bool hasPosition = readVec3(camera->find("position"), position);
        bool hasTarget = readVec3(camera->find("target"), target);
        if (const sj::Value* orbit = camera->find("orbit"); orbit != nullptr && orbit->isObject()) {
            Vec3 center{0.0, 0.0, 0.0};
            double radius = 0.0;
            double azimuth = 0.0;
            double elevation = 0.0;
            if (!readVec3(orbit->find("center"), center) ||
                !asDouble(orbit->find("radius"), radius) || radius <= 0.0 ||
                !asDouble(orbit->find("azimuthDegrees"), azimuth) ||
                !asDouble(orbit->find("elevationDegrees"), elevation)) {
                addError(diagnostics, 0, "", "camera.orbit",
                         "orbit requires finite center, positive radius, azimuth and elevation");
            } else {
                const double az = radians(azimuth);
                const double el = radians(elevation);
                position = center + Vec3(
                    radius * std::cos(el) * std::sin(az),
                    radius * std::sin(el),
                    radius * std::cos(el) * std::cos(az));
                target = center;
                hasPosition = true;
                hasTarget = true;
            }
        }
        if (!hasPosition) {
            addError(diagnostics, 0, "", "camera.position",
                     "camera requires a finite position or valid orbit");
        }
        if (!hasTarget) {
            addError(diagnostics, 0, "", "camera.target", "camera target must be finite");
        }
        if (const sj::Value* upValue = camera->find("up"); upValue != nullptr) {
            if (!readVec3(upValue, up) || length(up) <= kEpsilon) {
                addError(diagnostics, 0, "", "camera.up",
                         "camera up must be a non-zero finite vector");
            }
        }
        double fov = 0.0;
        if (!asDouble(key(*camera, {"fovY", "fovYDegrees", "verticalFov", "fov"}), fov) ||
            fov <= 1.0 || fov >= 179.0) {
            addError(diagnostics, 0, "", "camera.fovY",
                     "camera vertical FOV must be within (1,179) degrees");
        }
        double nearPlane = 0.01;
        double farPlane = 10000.0;
        if (const sj::Value* nearValue = camera->find("near"); nearValue != nullptr) {
            if (!asDouble(nearValue, nearPlane) || nearPlane <= 0.0) {
                addError(diagnostics, 0, "", "camera.near",
                         "camera near plane must be finite and positive");
            }
        }
        if (const sj::Value* farValue = camera->find("far"); farValue != nullptr) {
            if (!asDouble(farValue, farPlane) || farPlane <= nearPlane) {
                addError(diagnostics, 0, "", "camera.far",
                         "camera far plane must be finite and greater than near");
            }
        }
        scene.camera.position = position;
        scene.camera.target = target;
        scene.camera.up = up;
        scene.camera.fovYDegrees = fov;
        scene.camera.nearPlane = nearPlane;
        scene.camera.farPlane = farPlane;
        if (length(position - target) <= 1.0e-9) {
            addError(diagnostics, 0, "", "camera.position",
                     "camera position and target must differ");
        }
        if (length(cross(up, target - position)) <= 1.0e-9) {
            addError(diagnostics, 0, "", "camera.up",
                     "camera up vector must not be parallel to the view direction");
        }
    }

    const sj::Value* moire = root.find("moire");
    if (moire != nullptr) {
        if (!moire->isObject()) {
            addError(diagnostics, 0, "", "moire", "moire configuration must be an object");
        } else {
            double threshold = scene.moire.thresholdDegrees;
            double strength = scene.moire.maskStrength;
            if (const sj::Value* t = key(*moire, {"thresholdDegrees", "angleThresholdDegrees"});
                t != nullptr) {
                if (!asDouble(t, threshold) || threshold < 0.0 || threshold > 90.0) {
                    addError(diagnostics, 0, "", "moire.thresholdDegrees",
                             "moire threshold must be within [0,90] degrees");
                }
            }
            if (const sj::Value* s = key(*moire, {"maskStrength", "suppressionStrength"});
                s != nullptr) {
                if (!asDouble(s, strength) || strength < 0.0 || strength > 1.0) {
                    addError(diagnostics, 0, "", "moire.maskStrength",
                             "moire mask strength must be within [0,1]");
                }
            }
            scene.moire.thresholdDegrees = threshold;
            scene.moire.maskStrength = strength;
        }
    }

    if (lights != nullptr && lights->isArray()) {
        for (const ScreenLayer& layer : scene.layers) {
            if (layer.triangleData.empty()) continue;
            for (std::size_t i = 0; i < 2; ++i) {
                const StripLight& light = scene.lights[i];
                if (segmentOverlapsLayer(layer, light.a, light.b) ||
                    lightOverlapsLayer(layer, light.midpoint)) {
                    addError(diagnostics, layer.layerIndex, layer.separationId,
                             "lights[" + std::to_string(i) + "].position",
                             "light strip lies inside screen layer geometry");
                }
            }
        }
    }

    if (!diagnostics.empty()) {
        return false;
    }

    std::sort(scene.layers.begin(), scene.layers.end(),
              [](const ScreenLayer& a, const ScreenLayer& b) {
                  return a.layerIndex < b.layerIndex;
              });
    applyDeclaredAirGaps(scene);
    for (std::size_t i = 0; i + 1 < scene.layers.size() && i < scene.airGaps.size(); ++i) {
        scene.layers[i].airGapAbove = scene.airGaps[i];
    }

    sj::Value summary = sj::Value::object();
    summary["name"] = sj::Value(scene.name);
    summary["version"] = sj::Value(scene.version);
    summary["sceneDigest"] = sj::Value(scene.sceneDigest);
    summary["image"] = sj::Value::object();
    summary["image"]["width"] = sj::Value(scene.image.width);
    summary["image"]["height"] = sj::Value(scene.image.height);
    summary["image"]["blockWidth"] = sj::Value(scene.image.blockWidth);
    summary["image"]["blockHeight"] = sj::Value(scene.image.blockHeight);
    summary["layers"] = sj::Value::array();
    for (const ScreenLayer& layer : scene.layers) {
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(layer.layerIndex);
        item["separationId"] = sj::Value(layer.separationId);
        item["lineCount"] = sj::Value(layer.meshLineCount);
        item["angleDegrees"] = sj::Value(layer.meshAngleDegrees);
        item["seed"] = sj::Value(static_cast<double>(layer.seed));
        item["halftoneDigest"] = sj::Value(hex64(layer.halftone.digest()));
        summary["layers"].push(std::move(item));
    }
    scene.sceneSummaryJson = summary.dump();
    return true;
}

bool loadScene(const std::string& path, Scene& scene, std::vector<Diagnostic>& diagnostics) {
    diagnostics.clear();
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        addError(diagnostics, 0, "", "$", "cannot open scene JSON: " + path);
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    std::string error;
    sj::Value root = sj::Value::parse(buffer.str(), error);
    if (!error.empty()) {
        addError(diagnostics, 0, "", "$", error);
        return false;
    }
    return parseScene(root, scene, diagnostics);
}

sj::Value diagnosticsJson(const std::vector<Diagnostic>& diagnostics) {
    sj::Value result = sj::Value::object();
    result["ok"] = sj::Value(false);
    result["code"] = sj::Value("E_SCENE_INVALID");
    result["diagnostics"] = sj::Value::array();
    for (const Diagnostic& diagnostic : diagnostics) {
        sj::Value item = sj::Value::object();
        item["code"] = sj::Value(diagnostic.code);
        item["layer"] = sj::Value(diagnostic.layer);
        if (!diagnostic.separationId.empty()) {
            item["separationId"] = sj::Value(diagnostic.separationId);
        }
        item["path"] = sj::Value(diagnostic.path);
        item["message"] = sj::Value(diagnostic.message);
        result["diagnostics"].push(std::move(item));
    }
    return result;
}

void writeJsonFile(const std::string& path, const sj::Value& value, int indent) {
    std::filesystem::path target(path);
    if (!target.parent_path().empty()) {
        std::filesystem::create_directories(target.parent_path());
    }
    std::ofstream output(target, std::ios::binary | std::ios::trunc);
    if (!output) {
        throw std::runtime_error("cannot open JSON output: " + path);
    }
    output << value.dump(indent) << '\n';
}

sj::Value makeSampleScene(std::uint64_t seed) {
    sj::Value scene = sj::Value::object();
    scene["name"] = sj::Value("four-layer-backlit-silkscreen-validation");
    scene["version"] = sj::Value(1);
    scene["image"] = sj::Value::object();
    scene["image"]["width"] = sj::Value(384);
    scene["image"]["height"] = sj::Value(256);
    scene["image"]["blockSize"] = sj::Value(32);
    scene["image"]["exposure"] = sj::Value(1.0);

    scene["screens"] = sj::Value::array();
    scene["screens"].push(makeLayer(
        makeRectangle(-4.0, 0.0, 0.0, 124.0, 82.0),
        1, "CYAN-A", 0.72, 0.10, 0.66, 0.78, 46, 15.0, seed + 101ULL));
    scene["screens"].push(makeLayer(
        makeRectangle(8.0, 3.0, 1.2, 108.0, 70.0),
        2, "CYAN-B", 0.77, 0.04, 0.56, 0.86, 49, 16.4, seed + 202ULL));
    scene["screens"].push(makeLayer(
        makeHexagon(-7.0, -3.0, 2.8, 56.0, 34.0),
        3, "MAGENTA-A", 0.69, 0.82, 0.18, 0.62, 54, 75.0, seed + 303ULL));
    scene["screens"].push(makeLayer(
        makeRectangle(-13.0, -7.0, 3.7, 88.0, 58.0),
        4, "KEY-A", 0.64, 0.12, 0.12, 0.16, 61, 45.0, seed + 404ULL));

    scene["airGaps"] = sj::Value::array();
    scene["airGaps"].push(sj::Value(1.2));
    scene["airGaps"].push(sj::Value(1.6));
    scene["airGaps"].push(sj::Value(0.9));

    scene["lights"] = sj::Value::array();
    sj::Value lightA = sj::Value::object();
    lightA["position"] = vec3Json({-38.0, -30.0, -26.0});
    lightA["direction"] = vec3Json({1.0, 0.0, 0.0});
    lightA["length"] = sj::Value(44.0);
    lightA["intensity"] = sj::Value(2.6);
    lightA["color"] = vec3Json({1.0, 0.94, 0.82});
    scene["lights"].push(lightA);
    sj::Value lightB = sj::Value::object();
    lightB["position"] = vec3Json({32.0, 26.0, -29.0});
    lightB["direction"] = vec3Json({1.0, 0.0, 0.0});
    lightB["length"] = sj::Value(38.0);
    lightB["intensity"] = sj::Value(2.25);
    lightB["color"] = vec3Json({0.88, 0.95, 1.0});
    scene["lights"].push(lightB);

    sj::Value board = sj::Value::object();
    board["origin"] = vec3Json({0.0, 0.0, 18.0});
    board["normal"] = vec3Json({0.0, 0.0, 1.0});
    board["width"] = sj::Value(160.0);
    board["height"] = sj::Value(112.0);
    scene["backboard"] = board;

    sj::Value camera = sj::Value::object();
    camera["position"] = vec3Json({10.0, -5.0, -150.0});
    camera["target"] = vec3Json({0.0, 0.0, 1.0});
    camera["up"] = vec3Json({0.0, 1.0, 0.0});
    camera["fovY"] = sj::Value(38.0);
    camera["near"] = sj::Value(0.1);
    camera["far"] = sj::Value(1000.0);
    scene["camera"] = camera;

    scene["moire"] = sj::Value::object();
    scene["moire"]["thresholdDegrees"] = sj::Value(3.0);
    scene["moire"]["maskStrength"] = sj::Value(0.82);
    return scene;
}

} // namespace sl

