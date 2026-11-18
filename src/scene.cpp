#include "scene.h"

#include <algorithm>
#include <cmath>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <stdexcept>

#include "sha256.h"

namespace caisson {
namespace {

struct SceneException {
    StructuredError error;
};

[[noreturn]] void fail(
    const std::string& code,
    const std::string& message,
    int layerId = -1,
    int quadrant = -1,
    const std::string& objectId = "") {
    StructuredError error(code);
    error.message = message;
    error.layerId = layerId;
    error.quadrant = quadrant;
    error.objectId = objectId;
    throw SceneException{error};
}

const json::Value& required(const json::Value& parent, const char* key) {
    const json::Value* value = parent.find(key);
    if (value == nullptr) {
        fail("E_SCHEMA_FIELD", std::string("missing required field: ") + key);
    }
    return *value;
}

double number(const json::Value& value, const std::string& field) {
    if (!value.isNumber()) fail("E_SCHEMA_TYPE", "field must be a number: " + field);
    if (!std::isfinite(value.number)) fail("E_SCHEMA_TYPE", "field must be finite: " + field);
    return value.number;
}

std::int64_t integer(const json::Value& value, const std::string& field) {
    const double raw = number(value, field);
    if (raw < static_cast<double>(std::numeric_limits<std::int64_t>::min()) ||
        raw > static_cast<double>(std::numeric_limits<std::int64_t>::max()) ||
        std::floor(raw) != raw) {
        fail("E_SCHEMA_TYPE", "field must be an integer: " + field);
    }
    return static_cast<std::int64_t>(raw);
}

int int32Integer(const json::Value& value, const std::string& field) {
    const std::int64_t raw = integer(value, field);
    if (raw < std::numeric_limits<int>::min() ||
        raw > std::numeric_limits<int>::max()) {
        fail("E_SCHEMA_TYPE", "field is outside the 32-bit integer range: " + field);
    }
    return static_cast<int>(raw);
}

std::string stringValue(const json::Value& value, const std::string& field) {
    if (!value.isString() || value.string.empty()) {
        fail("E_SCHEMA_TYPE", "field must be a non-empty string: " + field);
    }
    return value.string;
}

Vec3 vector3(
    const json::Value& value,
    const std::string& field,
    int layerId = -1,
    int quadrant = -1) {
    if (!value.isArray() || value.array.size() != 3) {
        fail("E_SCHEMA_TYPE", "field must be an array of three numbers: " + field,
             layerId, quadrant);
    }
    return {
        number(value.array[0], field),
        number(value.array[1], field),
        number(value.array[2], field),
    };
}

double unitRange(
    const json::Value& value,
    const std::string& field,
    const std::string& code,
    int layerId = -1,
    int quadrant = -1) {
    const double output = number(value, field);
    if (output < 0.0 || output > 1.0) {
        fail(code, field + " must be in [0, 1]", layerId, quadrant);
    }
    return output;
}

void parseMesh(
    const json::Value& object,
    Mesh& mesh,
    int layerId,
    int quadrant,
    const std::string& objectId) {
    const json::Value& grid = required(object, "grid");
    if (!grid.isObject()) fail("E_GRID_INVALID", "grid must be an object", layerId, quadrant);
    const int columns = int32Integer(required(grid, "columns"), "grid.columns");
    const int rows = int32Integer(required(grid, "rows"), "grid.rows");
    if (columns < 1 || rows < 1 || columns > 1024 || rows > 1024) {
        fail("E_GRID_INVALID", "grid columns/rows must be in [1, 1024]",
             layerId, quadrant);
    }
    mesh.columns = columns;
    mesh.rows = rows;

    const json::Value& vertices = required(object, "vertices");
    if (!vertices.isArray() || vertices.array.empty()) {
        fail("E_GEOMETRY_EMPTY", "vertices must be a non-empty array",
             layerId, quadrant, objectId);
    }
    mesh.positions.reserve(vertices.array.size());
    for (const json::Value& vertex : vertices.array) {
        mesh.positions.push_back(vector3(vertex, "vertices[]", layerId, quadrant));
    }

    const json::Value* uvs = object.find("uvs");
    if (uvs != nullptr) {
        if (!uvs->isArray() || uvs->array.size() != mesh.positions.size()) {
            fail("E_UV_COUNT", "uvs count must equal vertex count",
                 layerId, quadrant, objectId);
        }
        mesh.uvs.reserve(uvs->array.size());
        for (const json::Value& uv : uvs->array) {
            if (!uv.isArray() || uv.array.size() != 2) {
                fail("E_UV_COUNT", "each uv must contain two numbers",
                     layerId, quadrant, objectId);
            }
            mesh.uvs.push_back({
                number(uv.array[0], "uvs[]"),
                number(uv.array[1], "uvs[]"),
            });
        }
    } else {
        double minX = mesh.positions[0].x;
        double maxX = minX;
        double minZ = mesh.positions[0].z;
        double maxZ = minZ;
        for (const Vec3& position : mesh.positions) {
            minX = std::min(minX, position.x);
            maxX = std::max(maxX, position.x);
            minZ = std::min(minZ, position.z);
            maxZ = std::max(maxZ, position.z);
        }
        const double dx = std::max(maxX - minX, 1.0e-12);
        const double dz = std::max(maxZ - minZ, 1.0e-12);
        mesh.uvs.reserve(mesh.positions.size());
        for (const Vec3& position : mesh.positions) {
            mesh.uvs.emplace_back((position.x - minX) / dx, (position.z - minZ) / dz);
        }
    }

    const json::Value& indices = required(object, "indices");
    if (!indices.isArray() || indices.array.empty() ||
        indices.array.size() % 3 != 0) {
        fail("E_GEOMETRY_EMPTY", "indices must contain a non-empty multiple of three",
             layerId, quadrant, objectId);
    }
    mesh.indices.reserve(indices.array.size());
    for (const json::Value& index : indices.array) {
        const std::int64_t raw = integer(index, "indices[]");
        if (raw < 0 || static_cast<std::size_t>(raw) >= mesh.positions.size()) {
            fail("E_GEOMETRY_INDEX", "triangle index is out of range",
                 layerId, quadrant, objectId);
        }
        mesh.indices.push_back(static_cast<std::uint32_t>(raw));
    }

    for (std::size_t i = 0; i < mesh.indices.size(); i += 3) {
        const Vec3& a = mesh.positions[mesh.indices[i]];
        const Vec3& b = mesh.positions[mesh.indices[i + 1]];
        const Vec3& c = mesh.positions[mesh.indices[i + 2]];
        if (lengthSquared(cross(b - a, c - a)) <= 1.0e-18) {
            fail("E_TRIANGLE_DEGENERATE", "triangle has zero area",
                 layerId, quadrant, objectId);
        }
    }
}

void parseCamera(const json::Value& object, Camera& camera) {
    camera.position = vector3(required(object, "position"), "camera.position");
    camera.target = vector3(required(object, "target"), "camera.target");
    camera.up = vector3(required(object, "up"), "camera.up");
    camera.verticalFovDegrees =
        number(required(object, "vertical_fov_degrees"), "camera.vertical_fov_degrees");
    camera.nearZ = number(required(object, "near"), "camera.near");
    camera.farZ = number(required(object, "far"), "camera.far");

    if (lengthSquared(camera.target - camera.position) <= 1.0e-16) {
        fail("E_CAMERA_PARAM", "camera position and target must differ");
    }
    if (lengthSquared(camera.up) <= 1.0e-16) {
        fail("E_CAMERA_PARAM", "camera up vector must be non-zero");
    }
    if (camera.verticalFovDegrees <= 1.0 || camera.verticalFovDegrees >= 179.0) {
        fail("E_CAMERA_PARAM", "vertical_fov_degrees must be in (1, 179)");
    }
    if (camera.nearZ <= 0.0 || camera.farZ <= camera.nearZ) {
        fail("E_CAMERA_PARAM", "camera near/far must satisfy 0 < near < far");
    }

    camera.forward = normalize(camera.target - camera.position);
    camera.right = normalize(cross(camera.forward, camera.up));
    if (lengthSquared(camera.right) <= 1.0e-16) {
        fail("E_CAMERA_PARAM", "camera up vector is parallel to view direction");
    }
    camera.correctedUp = normalize(cross(camera.right, camera.forward));
}

void computeLayerPlane(Layer& layer) {
    Vec3 sum{};
    Vec3 weightedNormal{};
    std::size_t count = 0;
    for (const QuadrantMesh& quadrant : layer.quadrants) {
        for (const Vec3& position : quadrant.mesh.positions) {
            sum += position;
            ++count;
        }
        for (std::size_t i = 0; i < quadrant.mesh.indices.size(); i += 3) {
            const Vec3& a = quadrant.mesh.positions[quadrant.mesh.indices[i]];
            const Vec3& b = quadrant.mesh.positions[quadrant.mesh.indices[i + 1]];
            const Vec3& c = quadrant.mesh.positions[quadrant.mesh.indices[i + 2]];
            weightedNormal += cross(b - a, c - a);
        }
    }
    if (count == 0 || lengthSquared(weightedNormal) <= 1.0e-20) {
        fail("E_LAYER_DEGENERATE", "layer has no usable plane", layer.id);
    }
    layer.planePoint = sum / static_cast<double>(count);
    layer.planeNormal = normalize(weightedNormal);
}

bool pointInTriangle(
    const Vec3& point,
    const Vec3& a,
    const Vec3& b,
    const Vec3& c) {
    const Vec3 v0 = b - a;
    const Vec3 v1 = c - a;
    const Vec3 v2 = point - a;
    const double d00 = dot(v0, v0);
    const double d01 = dot(v0, v1);
    const double d11 = dot(v1, v1);
    const double d20 = dot(v2, v0);
    const double d21 = dot(v2, v1);
    const double denom = d00 * d11 - d01 * d01;
    if (std::abs(denom) <= 1.0e-20) return false;
    const double v = (d11 * d20 - d01 * d21) / denom;
    const double w = (d00 * d21 - d01 * d20) / denom;
    const double u = 1.0 - v - w;
    constexpr double tolerance = 1.0e-8;
    return u >= -tolerance && v >= -tolerance && w >= -tolerance;
}

void validateLampsOutsideLayers(const Scene& scene) {
    for (const Layer& layer : scene.layers) {
        for (const QuadrantMesh& quadrant : layer.quadrants) {
            for (std::size_t i = 0; i < quadrant.mesh.indices.size(); i += 3) {
                const Vec3& a = quadrant.mesh.positions[quadrant.mesh.indices[i]];
                const Vec3& b = quadrant.mesh.positions[quadrant.mesh.indices[i + 1]];
                const Vec3& c = quadrant.mesh.positions[quadrant.mesh.indices[i + 2]];
                const Vec3 normal = normalize(cross(b - a, c - a));
                for (const Lamp& lamp : scene.lamps) {
                    const double distance = std::abs(dot(lamp.position - a, normal));
                    if (distance <= 1.0e-5 &&
                        pointInTriangle(lamp.position, a, b, c)) {
                        fail("E_LAMP_INSIDE_LAYER",
                             "lamp lies inside layer geometry",
                             layer.id, quadrant.quadrant, lamp.id);
                    }
                }
            }
        }
    }
}

std::string canonicalMesh(const Mesh& mesh) {
    std::ostringstream out;
    out << "grid=" << mesh.columns << ',' << mesh.rows << ";v=";
    for (const Vec3& p : mesh.positions) {
        out << formatDouble(p.x) << ',' << formatDouble(p.y) << ','
            << formatDouble(p.z) << ';';
    }
    out << "uv=";
    for (const Vec2& uv : mesh.uvs) {
        out << formatDouble(uv.x) << ',' << formatDouble(uv.y) << ';';
    }
    out << "i=";
    for (const std::uint32_t index : mesh.indices) out << index << ',';
    return out.str();
}

}  // namespace

bool loadScene(
    const std::filesystem::path& path,
    Scene& scene,
    StructuredError& error) {
    try {
        std::string text;
        std::string ioError;
        if (!readTextFile(path, text, ioError)) {
            error = StructuredError("E_SCENE_IO");
            error.message = ioError;
            return false;
        }

        json::Value root;
        try {
            root = json::parse(text);
        } catch (const json::ParseError& parseError) {
            error = StructuredError("E_JSON_PARSE");
            error.message = "JSON parse error at line " +
                            std::to_string(parseError.line) + ", column " +
                            std::to_string(parseError.column) + ": " +
                            parseError.what();
            return false;
        }
        if (!root.isObject()) fail("E_SCHEMA_TYPE", "scene root must be an object");

        scene = Scene{};
        scene.schemaVersion =
            int32Integer(required(root, "schema_version"), "schema_version");
        if (scene.schemaVersion != 1) {
            fail("E_SCHEMA_VERSION", "only schema_version 1 is supported");
        }
        scene.seed = integer(required(root, "seed"), "seed");
        if (scene.seed < -9007199254740991LL ||
            scene.seed > 9007199254740991LL) {
            fail("E_SEED_RANGE", "seed must be a safe signed 64-bit integer");
        }

        const json::Value& image = required(root, "image");
        if (!image.isObject()) fail("E_SCHEMA_TYPE", "image must be an object");
        scene.image.width = int32Integer(required(image, "width"), "image.width");
        scene.image.height = int32Integer(required(image, "height"), "image.height");
        scene.image.tileSize =
            int32Integer(required(image, "tile_size"), "image.tile_size");
        scene.image.aaSamples =
            int32Integer(required(image, "aa_samples"), "image.aa_samples");
        if (scene.image.width < 1 || scene.image.height < 1 ||
            scene.image.width > 16384 || scene.image.height > 16384 ||
            static_cast<std::int64_t>(scene.image.width) * scene.image.height > 67108864LL) {
            fail("E_IMAGE_SIZE", "image dimensions are invalid or too large");
        }
        if (scene.image.tileSize < 1 || scene.image.tileSize > 4096) {
            fail("E_TILE_SIZE", "image.tile_size must be in [1, 4096]");
        }
        if (scene.image.aaSamples < 1 || scene.image.aaSamples > 4) {
            fail("E_AA_RANGE", "image.aa_samples must be in [1, 4]");
        }
        if (const json::Value* background = image.find("background")) {
            scene.image.background = vector3(*background, "image.background");
            if (scene.image.background.x < 0.0 || scene.image.background.y < 0.0 ||
                scene.image.background.z < 0.0) {
                fail("E_COLOR_RANGE", "image.background must be non-negative");
            }
        }

        const json::Value& camera = required(root, "camera");
        if (!camera.isObject()) fail("E_SCHEMA_TYPE", "camera must be an object");
        parseCamera(camera, scene.camera);

        const json::Value& layers = required(root, "layers");
        if (!layers.isArray() || layers.array.empty()) {
            fail("E_LAYER_EMPTY", "layers must be a non-empty array");
        }
        std::set<int> layerIds;
        for (const json::Value& layerValue : layers.array) {
            if (!layerValue.isObject()) fail("E_SCHEMA_TYPE", "layer must be an object");
            Layer layer;
            layer.id = int32Integer(required(layerValue, "layer_id"), "layer_id");
            if (layer.id < 1 || layer.id > 254) {
                fail("E_LAYER_ID_RANGE", "layer_id must be in [1, 254]", layer.id);
            }
            if (!layerIds.insert(layer.id).second) {
                fail("E_LAYER_DUPLICATE", "duplicate layer_id", layer.id);
            }
            layer.woodColor =
                vector3(required(layerValue, "wood_color"), "wood_color", layer.id);
            if (layer.woodColor.x < 0.0 || layer.woodColor.x > 1.0 ||
                layer.woodColor.y < 0.0 || layer.woodColor.y > 1.0 ||
                layer.woodColor.z < 0.0 || layer.woodColor.z > 1.0) {
                fail("E_COLOR_RANGE", "wood_color components must be in [0, 1]", layer.id);
            }
            layer.transmittance = unitRange(
                required(layerValue, "transmittance"),
                "transmittance",
                "E_TRANSMITTANCE_RANGE",
                layer.id);
            layer.reflectance = unitRange(
                required(layerValue, "reflectance"),
                "reflectance",
                "E_REFLECTANCE_RANGE",
                layer.id);
            if (const json::Value* lattice = layerValue.find("lattice_width")) {
                layer.latticeWidth = number(*lattice, "lattice_width");
                if (layer.latticeWidth <= 0.001 || layer.latticeWidth >= 0.49) {
                    fail("E_LATTICE_RANGE", "lattice_width must be in (0.001, 0.49)",
                         layer.id);
                }
            }

            const json::Value* quadrants = layerValue.find("quadrants");
            std::set<int> quadrantIds;
            if (quadrants != nullptr) {
                if (!quadrants->isArray() || quadrants->array.empty()) {
                    fail("E_GEOMETRY_EMPTY", "quadrants must be a non-empty array",
                         layer.id);
                }
                for (const json::Value& quadrantValue : quadrants->array) {
                    if (!quadrantValue.isObject()) {
                        fail("E_SCHEMA_TYPE", "quadrant must be an object", layer.id);
                    }
                    QuadrantMesh quadrant;
                    quadrant.quadrant = int32Integer(
                        required(quadrantValue, "quadrant"), "quadrant");
                    if (quadrant.quadrant < 0 || quadrant.quadrant > 3) {
                        fail("E_QUADRANT_RANGE", "quadrant must be in [0, 3]",
                             layer.id, quadrant.quadrant);
                    }
                    if (!quadrantIds.insert(quadrant.quadrant).second) {
                        fail("E_QUADRANT_DUPLICATE", "duplicate quadrant in layer",
                             layer.id, quadrant.quadrant);
                    }
                    parseMesh(
                        quadrantValue,
                        quadrant.mesh,
                        layer.id,
                        quadrant.quadrant,
                        "");
                    layer.quadrants.push_back(std::move(quadrant));
                }
            } else {
                QuadrantMesh quadrant;
                if (const json::Value* q = layerValue.find("quadrant")) {
                    quadrant.quadrant = int32Integer(*q, "quadrant");
                }
                if (quadrant.quadrant < 0 || quadrant.quadrant > 3) {
                    fail("E_QUADRANT_RANGE", "quadrant must be in [0, 3]",
                         layer.id, quadrant.quadrant);
                }
                parseMesh(layerValue, quadrant.mesh, layer.id, quadrant.quadrant, "");
                layer.quadrants.push_back(std::move(quadrant));
            }
            std::sort(
                layer.quadrants.begin(),
                layer.quadrants.end(),
                [](const QuadrantMesh& a, const QuadrantMesh& b) {
                    return a.quadrant < b.quadrant;
                });
            computeLayerPlane(layer);
            scene.layers.push_back(std::move(layer));
        }
        std::sort(scene.layers.begin(), scene.layers.end(),
                  [](const Layer& a, const Layer& b) { return a.id < b.id; });

        const json::Value& lamps = required(root, "lamps");
        if (!lamps.isArray() || lamps.array.size() != 3) {
            fail("E_LAMP_COUNT", "exactly three lamps are required");
        }
        std::set<std::string> lampIds;
        for (const json::Value& lampValue : lamps.array) {
            if (!lampValue.isObject()) fail("E_SCHEMA_TYPE", "lamp must be an object");
            Lamp lamp;
            lamp.id = stringValue(required(lampValue, "id"), "lamp.id");
            if (!lampIds.insert(lamp.id).second) {
                fail("E_OBJECT_ID_DUPLICATE", "duplicate lamp id", -1, -1, lamp.id);
            }
            lamp.position = vector3(required(lampValue, "position"), "lamp.position");
            lamp.color = vector3(required(lampValue, "color"), "lamp.color");
            if (lamp.color.x < 0.0 || lamp.color.x > 1.0 ||
                lamp.color.y < 0.0 || lamp.color.y > 1.0 ||
                lamp.color.z < 0.0 || lamp.color.z > 1.0) {
                fail("E_COLOR_RANGE", "lamp color must be in [0, 1]", -1, -1, lamp.id);
            }
            lamp.intensity = number(required(lampValue, "intensity"), "lamp.intensity");
            lamp.radius = number(required(lampValue, "radius"), "lamp.radius");
            if (lamp.intensity < 0.0 || lamp.intensity > 1000.0) {
                fail("E_LAMP_PARAM", "lamp intensity must be in [0, 1000]", -1, -1, lamp.id);
            }
            if (lamp.radius <= 0.0 || lamp.radius > 10.0) {
                fail("E_LAMP_PARAM", "lamp radius must be in (0, 10]", -1, -1, lamp.id);
            }
            scene.lamps.push_back(std::move(lamp));
        }
        std::sort(scene.lamps.begin(), scene.lamps.end(),
                  [](const Lamp& a, const Lamp& b) { return a.id < b.id; });

        const json::Value& occluders = required(root, "dougong");
        if (!occluders.isArray() || occluders.array.empty()) {
            fail("E_GEOMETRY_EMPTY", "dougong must be a non-empty array");
        }
        std::set<std::string> occluderIds;
        for (const json::Value& occluderValue : occluders.array) {
            if (!occluderValue.isObject()) {
                fail("E_SCHEMA_TYPE", "dougong entry must be an object");
            }
            Occluder occluder;
            occluder.id = stringValue(
                required(occluderValue, "id"), "dougong.id");
            if (!occluderIds.insert(occluder.id).second) {
                fail("E_OBJECT_ID_DUPLICATE", "duplicate dougong id",
                     -1, -1, occluder.id);
            }
            parseMesh(occluderValue, occluder.mesh, -1, -1, occluder.id);
            if (const json::Value* color = occluderValue.find("color")) {
                occluder.color = vector3(*color, "dougong.color");
                if (occluder.color.x < 0.0 || occluder.color.x > 1.0 ||
                    occluder.color.y < 0.0 || occluder.color.y > 1.0 ||
                    occluder.color.z < 0.0 || occluder.color.z > 1.0) {
                    fail("E_COLOR_RANGE", "dougong color must be in [0, 1]",
                         -1, -1, occluder.id);
                }
            }
            Vec3 centroid{};
            for (const Vec3& position : occluder.mesh.positions) centroid += position;
            occluder.centroid = centroid / static_cast<double>(occluder.mesh.positions.size());
            scene.occluders.push_back(std::move(occluder));
        }
        std::sort(scene.occluders.begin(), scene.occluders.end(),
                  [](const Occluder& a, const Occluder& b) { return a.id < b.id; });

        const json::Value& shadow = required(root, "shadow");
        if (!shadow.isObject()) fail("E_SCHEMA_TYPE", "shadow must be an object");
        scene.shadow.samplesPerAxis = int32Integer(
            required(shadow, "samples_per_axis"), "shadow.samples_per_axis");
        scene.shadow.probeSamples = int32Integer(
            required(shadow, "probe_samples"), "shadow.probe_samples");
        scene.shadow.probeHalfSpan =
            number(required(shadow, "probe_half_span"), "shadow.probe_half_span");
        if (scene.shadow.samplesPerAxis < 2 || scene.shadow.samplesPerAxis > 16) {
            fail("E_SHADOW_SAMPLES", "shadow.samples_per_axis must be in [2, 16]");
        }
        if (scene.shadow.probeSamples < 5 || scene.shadow.probeSamples > 257 ||
            scene.shadow.probeSamples % 2 == 0) {
            fail("E_SHADOW_SAMPLES", "shadow.probe_samples must be odd in [5, 257]");
        }
        if (scene.shadow.probeHalfSpan <= 0.01 ||
            scene.shadow.probeHalfSpan > 20.0) {
            fail("E_SHADOW_SAMPLES", "shadow.probe_half_span must be in (0.01, 20]");
        }

        if (const json::Value* ambient = root.find("ambient")) {
            scene.ambient = vector3(*ambient, "ambient");
            if (scene.ambient.x < 0.0 || scene.ambient.y < 0.0 ||
                scene.ambient.z < 0.0) {
                fail("E_COLOR_RANGE", "ambient must be non-negative");
            }
        }

        validateLampsOutsideLayers(scene);
        scene.digest = computeSceneDigest(scene);
        return true;
    } catch (const SceneException& exception) {
        error = exception.error;
        return false;
    } catch (const std::exception& exception) {
        error = StructuredError("E_SCENE_INTERNAL");
        error.message = exception.what();
        return false;
    }
}

void validateSceneInvariants(const Scene& scene, StructuredError& error) {
    if (scene.layers.empty() || scene.lamps.size() != 3 || scene.occluders.empty()) {
        error = StructuredError("E_SCENE_INTERNAL");
        error.message = "scene is not fully initialized";
        return;
    }
    error = StructuredError("E_NONE");
}

std::string computeSceneDigest(const Scene& scene) {
    std::ostringstream out;
    out << "caisson-scene-v1|" << scene.schemaVersion << '|' << scene.seed << '|';
    out << scene.image.width << ',' << scene.image.height << ','
        << scene.image.tileSize << ',' << scene.image.aaSamples << ','
        << formatDouble(scene.image.background.x) << ','
        << formatDouble(scene.image.background.y) << ','
        << formatDouble(scene.image.background.z) << '|';
    out << formatDouble(scene.camera.position.x) << ','
        << formatDouble(scene.camera.position.y) << ','
        << formatDouble(scene.camera.position.z) << ';'
        << formatDouble(scene.camera.target.x) << ','
        << formatDouble(scene.camera.target.y) << ','
        << formatDouble(scene.camera.target.z) << ';'
        << formatDouble(scene.camera.up.x) << ','
        << formatDouble(scene.camera.up.y) << ','
        << formatDouble(scene.camera.up.z) << ';'
        << formatDouble(scene.camera.verticalFovDegrees) << ','
        << formatDouble(scene.camera.nearZ) << ','
        << formatDouble(scene.camera.farZ) << '|';
    out << formatDouble(scene.ambient.x) << ','
        << formatDouble(scene.ambient.y) << ','
        << formatDouble(scene.ambient.z) << '|';
    for (const Layer& layer : scene.layers) {
        out << "L" << layer.id << '|'
            << formatDouble(layer.woodColor.x) << ','
            << formatDouble(layer.woodColor.y) << ','
            << formatDouble(layer.woodColor.z) << ','
            << formatDouble(layer.transmittance) << ','
            << formatDouble(layer.reflectance) << ','
            << formatDouble(layer.latticeWidth) << '|';
        for (const QuadrantMesh& quadrant : layer.quadrants) {
            out << "Q" << quadrant.quadrant << '|'
                << canonicalMesh(quadrant.mesh) << '|';
        }
    }
    for (const Lamp& lamp : scene.lamps) {
        out << "P" << lamp.id << '|'
            << formatDouble(lamp.position.x) << ','
            << formatDouble(lamp.position.y) << ','
            << formatDouble(lamp.position.z) << ','
            << formatDouble(lamp.color.x) << ','
            << formatDouble(lamp.color.y) << ','
            << formatDouble(lamp.color.z) << ','
            << formatDouble(lamp.intensity) << ','
            << formatDouble(lamp.radius) << '|';
    }
    for (const Occluder& occluder : scene.occluders) {
        out << "D" << occluder.id << '|'
            << formatDouble(occluder.color.x) << ','
            << formatDouble(occluder.color.y) << ','
            << formatDouble(occluder.color.z) << '|'
            << canonicalMesh(occluder.mesh) << '|';
    }
    out << "S" << scene.shadow.samplesPerAxis << ','
        << scene.shadow.probeSamples << ','
        << formatDouble(scene.shadow.probeHalfSpan);
    return sha256Hex(out.str());
}

}  // namespace caisson

