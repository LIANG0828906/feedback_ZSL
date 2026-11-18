#include "scene.hpp"

#include "sha256.hpp"

#include <algorithm>
#include <cmath>
#include <limits>
#include <map>
#include <set>
#include <sstream>

namespace sr {
namespace {

constexpr uint32_t kMaxParticleId = 0xfffffffeU;
constexpr int kMaxImageDimension = 4096;
constexpr int kMaxLayers = 256;
constexpr int kMaxCameraSamples = 100000;
constexpr int kMaxParticles = 100000;
constexpr int kMaxLights = 64;

std::string indexPath(const std::string& collection, int index) {
    return collection + "[" + std::to_string(index) + "]";
}

void addError(std::vector<ValidationError>& errors, const std::string& code,
              const std::string& path, const std::string& message,
              const std::string& collection = std::string(), int index = -1,
              const std::string& field = std::string()) {
    ValidationError error;
    error.code = code;
    error.path = path;
    error.message = message;
    error.element.collection = collection;
    error.element.index = index;
    error.element.field = field;
    errors.push_back(std::move(error));
}

const Json* getObjectField(const Json& object, const std::string& key,
                           const std::string& path,
                           std::vector<ValidationError>& errors) {
    const Json* field = object.get(key);
    if (field == nullptr) {
        addError(errors, "MISSING_REQUIRED_FIELD", path, "required object is missing", {}, -1,
                 key);
        return nullptr;
    }
    if (!field->isObject()) {
        addError(errors, "INVALID_TYPE", path, "expected object", {}, -1, key);
        return nullptr;
    }
    return field;
}

const Json* getArrayField(const Json& object, const std::string& key,
                          const std::string& path,
                          std::vector<ValidationError>& errors) {
    const Json* field = object.get(key);
    if (field == nullptr) {
        addError(errors, "MISSING_REQUIRED_FIELD", path, "required array is missing", {}, -1,
                 key);
        return nullptr;
    }
    if (!field->isArray()) {
        addError(errors, "INVALID_TYPE", path, "expected array", {}, -1, key);
        return nullptr;
    }
    return field;
}

bool numberField(const Json& object, const std::string& key, const std::string& path,
                 double& out, std::vector<ValidationError>& errors, bool required = true,
                 double fallback = 0.0,
                 const std::string& collection = std::string(), int index = -1) {
    const Json* field = object.get(key);
    if (field == nullptr) {
        if (required) {
            addError(errors, "MISSING_REQUIRED_FIELD", path,
                     "required numeric field is missing", collection, index, key);
        }
        out = fallback;
        return false;
    }
    if (!field->isNumber() || !isFinite(field->asNumber())) {
        addError(errors, "INVALID_NUMBER", path, "expected a finite JSON number", collection,
                 index, key);
        out = fallback;
        return false;
    }
    out = field->asNumber();
    return true;
}

bool integerField(const Json& object, const std::string& key, const std::string& path,
                  int64_t& out, std::vector<ValidationError>& errors, bool required = true,
                  int64_t fallback = 0,
                  const std::string& collection = std::string(), int index = -1) {
    double value = static_cast<double>(fallback);
    if (!numberField(object, key, path, value, errors, required, static_cast<double>(fallback),
                     collection, index)) {
        out = fallback;
        return false;
    }
    if (value != std::floor(value) || value < -9007199254740992.0 ||
        value > 9007199254740992.0) {
        addError(errors, "INVALID_INTEGER", path, "expected an integer", collection, index, key);
        out = fallback;
        return false;
    }
    out = static_cast<int64_t>(value);
    return true;
}

bool vec2Field(const Json& object, const std::string& key, const std::string& path, Vec2& out,
               std::vector<ValidationError>& errors, bool required = true,
               const Vec2& fallback = Vec2{},
               const std::string& collection = std::string(), int index = -1) {
    const Json* field = object.get(key);
    if (field == nullptr) {
        if (required) {
            addError(errors, "MISSING_REQUIRED_FIELD", path,
                     "required two-element numeric array is missing", collection, index, key);
        }
        out = fallback;
        return false;
    }
    if (!field->isArray() || field->items().size() != 2U) {
        addError(errors, "INVALID_VECTOR", path, "expected [x, y]", collection, index, key);
        out = fallback;
        return false;
    }
    Vec2 value;
    if (!field->items()[0].isNumber() || !field->items()[1].isNumber()) {
        addError(errors, "INVALID_VECTOR", path, "vector elements must be finite numbers",
                 collection, index, key);
        out = fallback;
        return false;
    }
    value.x = field->items()[0].asNumber();
    value.y = field->items()[1].asNumber();
    if (!isFinite(value.x) || !isFinite(value.y)) {
        addError(errors, "NON_FINITE_VALUE", path, "vector contains a non-finite value",
                 collection, index, key);
        out = fallback;
        return false;
    }
    out = value;
    return true;
}

bool vec3Field(const Json& object, const std::string& key, const std::string& path, Vec3& out,
               std::vector<ValidationError>& errors, bool required = true,
               const Vec3& fallback = Vec3{},
               const std::string& collection = std::string(), int index = -1) {
    const Json* field = object.get(key);
    if (field == nullptr) {
        if (required) {
            addError(errors, "MISSING_REQUIRED_FIELD", path,
                     "required three-element numeric array is missing", collection, index, key);
        }
        out = fallback;
        return false;
    }
    if (!field->isArray() || field->items().size() != 3U) {
        addError(errors, "INVALID_VECTOR", path, "expected [x, y, z]", collection, index, key);
        out = fallback;
        return false;
    }
    Vec3 value;
    for (size_t i = 0; i < 3; ++i) {
        if (!field->items()[i].isNumber()) {
            addError(errors, "INVALID_VECTOR", path,
                     "vector elements must be finite numbers", collection, index, key);
            out = fallback;
            return false;
        }
    }
    value.x = field->items()[0].asNumber();
    value.y = field->items()[1].asNumber();
    value.z = field->items()[2].asNumber();
    if (!isFinite(value.x) || !isFinite(value.y) || !isFinite(value.z)) {
        addError(errors, "NON_FINITE_VALUE", path, "vector contains a non-finite value",
                 collection, index, key);
        out = fallback;
        return false;
    }
    out = value;
    return true;
}

void makeParticleBasis(Particle& particle) {
    const double c = std::cos(particle.orientation);
    const double s = std::sin(particle.orientation);
    particle.normal = normalize(Vec3{s, 0.0, c});
    particle.right = Vec3{c, 0.0, -s};
    particle.up = Vec3{0.0, 1.0, 0.0};
    const Vec3 halfRight = particle.right * (particle.width * 0.5);
    const Vec3 halfUp = particle.up * (particle.height * 0.5);
    particle.corners = {
        particle.center - halfRight - halfUp,
        particle.center + halfRight - halfUp,
        particle.center + halfRight + halfUp,
        particle.center - halfRight + halfUp,
    };
}

void makeGroundBasis(Ground& ground) {
    ground.normal = normalize(ground.normal);
    const Vec3 helper = std::abs(ground.normal.y) < 0.9 ? Vec3{0.0, 1.0, 0.0}
                                                          : Vec3{1.0, 0.0, 0.0};
    ground.uAxis = normalize(cross(helper, ground.normal));
    ground.vAxis = normalize(cross(ground.normal, ground.uAxis));
}

void validateGround(const Json& root, Scene& scene, std::vector<ValidationError>& errors) {
    const Json* ground = getObjectField(root, "ground", "ground", errors);
    if (ground == nullptr) {
        return;
    }
    vec3Field(*ground, "center", "ground.center", scene.ground.center, errors, true, {}, "ground",
              -1);
    const Json* normal = ground->get("normal");
    if (normal == nullptr) {
        addError(errors, "MISSING_REQUIRED_FIELD", "ground.normal",
                 "required normal is missing", "ground", -1, "normal");
    } else if (vec3Field(*ground, "normal", "ground.normal", scene.ground.normal, errors)) {
        if (length(scene.ground.normal) < 1e-8) {
            addError(errors, "GROUND_NORMAL_DEGENERATE", "ground.normal",
                     "ground normal must be non-zero", "ground", -1, "normal");
        }
    }
    Vec2 extent;
    if (vec2Field(*ground, "half_extent", "ground.half_extent", extent, errors, true, {},
                  "ground", -1)) {
        if (extent.x <= 1e-9 || extent.y <= 1e-9) {
            addError(errors, "GROUND_EXTENT_INVALID", "ground.half_extent",
                     "both ground half extents must be positive", "ground", -1,
                     "half_extent");
        } else {
            scene.ground.halfU = extent.x;
            scene.ground.halfV = extent.y;
        }
    }
    if (vec3Field(*ground, "albedo", "ground.albedo", scene.ground.albedo, errors, true, {},
                  "ground", -1)) {
        if (scene.ground.albedo.x < 0.0 || scene.ground.albedo.x > 1.0 ||
            scene.ground.albedo.y < 0.0 || scene.ground.albedo.y > 1.0 ||
            scene.ground.albedo.z < 0.0 || scene.ground.albedo.z > 1.0) {
            addError(errors, "GROUND_ALBEDO_OUT_OF_RANGE", "ground.albedo",
                     "albedo components must be in [0, 1]", "ground", -1, "albedo");
        }
    }
    if (length(scene.ground.normal) > 1e-8) {
        makeGroundBasis(scene.ground);
    }
}

void validateImage(const Json& root, Scene& scene, std::vector<ValidationError>& errors) {
    const Json* image = getObjectField(root, "image", "image", errors);
    if (image == nullptr) {
        return;
    }
    int64_t value = 0;
    if (integerField(*image, "width", "image.width", value, errors, true, 0, "image", -1)) {
        if (value < 1 || value > kMaxImageDimension) {
            addError(errors, "IMAGE_DIMENSION_INVALID", "image.width",
                     "width must be in [1, 4096]", "image", -1, "width");
        } else {
            scene.image.width = static_cast<int>(value);
        }
    }
    if (integerField(*image, "height", "image.height", value, errors, true, 0, "image", -1)) {
        if (value < 1 || value > kMaxImageDimension) {
            addError(errors, "IMAGE_DIMENSION_INVALID", "image.height",
                     "height must be in [1, 4096]", "image", -1, "height");
        } else {
            scene.image.height = static_cast<int>(value);
        }
    }
    if (integerField(*image, "tile_width", "image.tile_width", value, errors, true, 0, "image",
                     -1)) {
        if (value < 1 || value > kMaxImageDimension) {
            addError(errors, "TILE_OUT_OF_RANGE", "image.tile_width",
                     "tile width must be in [1, 4096]", "image", -1, "tile_width");
        } else {
            scene.image.tileWidth = static_cast<int>(value);
        }
    }
    if (integerField(*image, "tile_height", "image.tile_height", value, errors, true, 0, "image",
                     -1)) {
        if (value < 1 || value > kMaxImageDimension) {
            addError(errors, "TILE_OUT_OF_RANGE", "image.tile_height",
                     "tile height must be in [1, 4096]", "image", -1, "tile_height");
        } else {
            scene.image.tileHeight = static_cast<int>(value);
        }
    }
    if (integerField(*image, "pixel_samples", "image.pixel_samples", value, errors, false, 2,
                     "image", -1)) {
        if (value < 1 || value > 4) {
            addError(errors, "PIXEL_SAMPLE_DENSITY_INVALID", "image.pixel_samples",
                     "pixel_samples must be in [1, 4]", "image", -1, "pixel_samples");
        } else {
            scene.image.pixelSamples = static_cast<int>(value);
        }
    }
}

void validateRender(const Json& root, Scene& scene, std::vector<ValidationError>& errors) {
    const Json* render = getObjectField(root, "render", "render", errors);
    if (render == nullptr) {
        return;
    }
    int64_t integer = 0;
    if (!integerField(*render, "layer_count", "render.layer_count", integer, errors, true, 0,
                      "render", -1)) {
        // A missing count is already reported. Keep particles parseable and report a final
        // layer coverage error rather than dereferencing an invalid sentinel.
    } else if (integer < 1 || integer > kMaxLayers) {
        addError(errors, "LAYER_INDEX_OUT_OF_RANGE", "render.layer_count",
                 "layer_count must be in [1, 256]", "render", -1, "layer_count");
    } else {
        scene.render.layerCount = static_cast<int>(integer);
    }

    if (integerField(*render, "default_frame", "render.default_frame", integer, errors, false,
                     0, "render", -1)) {
        if (integer < 0) {
            addError(errors, "CAMERA_FRAME_INDEX_OUT_OF_RANGE", "render.default_frame",
                     "default_frame must be non-negative", "render", -1, "default_frame");
        } else {
            scene.render.defaultFrame = static_cast<int>(integer);
        }
    }
    if (integerField(*render, "shadow_samples", "render.shadow_samples", integer, errors, false,
                     9, "render", -1)) {
        if (integer < 2 || integer > 65) {
            addError(errors, "SHADOW_SAMPLE_DENSITY_INVALID", "render.shadow_samples",
                     "shadow_samples must be an integer in [2, 65]", "render", -1,
                     "shadow_samples");
        } else {
            scene.render.shadowSamples = static_cast<int>(integer);
        }
    }
    numberField(*render, "softness_scale", "render.softness_scale",
                scene.render.softnessScale, errors, false, 0.04, "render", -1);
    if (scene.render.softnessScale <= 0.0 || scene.render.softnessScale > 100.0) {
        addError(errors, "SOFTNESS_INVALID", "render.softness_scale",
                 "softness_scale must be in (0, 100]", "render", -1, "softness_scale");
    }
    numberField(*render, "exposure", "render.exposure", scene.render.exposure, errors, false,
                1.0, "render", -1);
    if (scene.render.exposure <= 0.0 || scene.render.exposure > 1000.0) {
        addError(errors, "EXPOSURE_INVALID", "render.exposure",
                 "exposure must be in (0, 1000]", "render", -1, "exposure");
    }
    if (vec3Field(*render, "ambient", "render.ambient", scene.render.ambient, errors, false,
                  Vec3{0.015, 0.018, 0.025}, "render", -1)) {
        if (scene.render.ambient.x < 0.0 || scene.render.ambient.y < 0.0 ||
            scene.render.ambient.z < 0.0) {
            addError(errors, "AMBIENT_OUT_OF_RANGE", "render.ambient",
                     "ambient components must be non-negative", "render", -1, "ambient");
        }
    }

    const Json* probes = render->get("shadow_probes");
    if (probes != nullptr) {
        if (!probes->isArray()) {
            addError(errors, "INVALID_TYPE", "render.shadow_probes",
                     "shadow_probes must be an array", "render", -1, "shadow_probes");
        } else {
            std::set<std::string> ids;
            for (size_t i = 0; i < probes->items().size(); ++i) {
                const Json& probeJson = probes->items()[i];
                const std::string path = "render.shadow_probes[" + std::to_string(i) + "]";
                if (!probeJson.isObject()) {
                    addError(errors, "INVALID_TYPE", path, "probe must be an object",
                             "render.shadow_probes", static_cast<int>(i), {});
                    continue;
                }
                ShadowProbe probe;
                const Json* id = probeJson.get("id");
                if (id == nullptr || !id->isString() || id->asString().empty()) {
                    addError(errors, "PROBE_ID_MISSING", path + ".id",
                             "probe id must be a non-empty string", "render.shadow_probes",
                             static_cast<int>(i), "id");
                } else {
                    probe.id = id->asString();
                    if (!ids.insert(probe.id).second) {
                        addError(errors, "PROBE_ID_DUPLICATE", path + ".id",
                                 "probe id is duplicated", "render.shadow_probes",
                                 static_cast<int>(i), "id");
                    }
                }
                int64_t lightId = 0;
                if (integerField(probeJson, "light_id", path + ".light_id", lightId, errors,
                                 true, 0, "render.shadow_probes", static_cast<int>(i))) {
                    if (lightId < 0 || static_cast<uint64_t>(lightId) > kMaxParticleId) {
                        addError(errors, "INDEX_OUT_OF_RANGE", path + ".light_id",
                                 "light_id is out of range", "render.shadow_probes",
                                 static_cast<int>(i), "light_id");
                    } else {
                        probe.lightId = static_cast<uint32_t>(lightId);
                    }
                }
                vec2Field(probeJson, "ground_uv", path + ".ground_uv", probe.groundUv, errors,
                          true, {}, "render.shadow_probes", static_cast<int>(i));
                scene.render.probes.push_back(probe);
            }
        }
    }
}

void validateParticles(const Json& root, Scene& scene,
                       std::vector<ValidationError>& errors) {
    const Json* particles = getArrayField(root, "particles", "particles", errors);
    if (particles == nullptr) {
        return;
    }
    if (particles->items().empty()) {
        addError(errors, "PARTICLE_COLLECTION_EMPTY", "particles",
                 "at least one particle is required");
    }
    if (particles->items().size() > static_cast<size_t>(kMaxParticles)) {
        addError(errors, "PARTICLE_COUNT_LIMIT", "particles",
                 "particle count exceeds 100000");
    }
    std::set<uint32_t> ids;
    std::vector<bool> layerPresent(static_cast<size_t>(std::max(scene.render.layerCount, 1)),
                                   false);
    for (size_t i = 0; i < particles->items().size(); ++i) {
        const Json& entry = particles->items()[i];
        const int index = static_cast<int>(i);
        const std::string path = indexPath("particles", index);
        if (!entry.isObject()) {
            addError(errors, "INVALID_TYPE", path, "particle must be an object", "particles",
                     index, {});
            continue;
        }
        Particle particle;
        int64_t id = 0;
        if (integerField(entry, "id", path + ".id", id, errors, true, 0, "particles", index)) {
            if (id < 0 || static_cast<uint64_t>(id) > kMaxParticleId) {
                addError(errors, "PARTICLE_ID_INVALID", path + ".id",
                         "particle id must be in [0, 4294967294]", "particles", index, "id");
            } else {
                particle.id = static_cast<uint32_t>(id);
                if (!ids.insert(particle.id).second) {
                    addError(errors, "PARTICLE_ID_DUPLICATE", path + ".id",
                             "particle id must be unique", "particles", index, "id");
                }
            }
        }
        int64_t layer = 0;
        if (integerField(entry, "layer", path + ".layer", layer, errors, true, 0, "particles",
                         index)) {
            if (scene.render.layerCount <= 0) {
                addError(errors, "LAYER_INDEX_OUT_OF_RANGE", path + ".layer",
                         "layer requires a valid render.layer_count", "particles", index,
                         "layer");
            } else if (layer < 0 || layer >= scene.render.layerCount) {
                addError(errors, "LAYER_INDEX_OUT_OF_RANGE", path + ".layer",
                         "layer must be in [0, layer_count)", "particles", index, "layer");
            } else {
                particle.layer = static_cast<int>(layer);
                layerPresent[static_cast<size_t>(layer)] = true;
            }
        }
        vec3Field(entry, "center", path + ".center", particle.center, errors, true, {},
                  "particles", index);
        Vec2 size;
        if (vec2Field(entry, "size", path + ".size", size, errors, true, {}, "particles",
                      index)) {
            if (size.x <= 1e-6 || size.y <= 1e-6) {
                addError(errors, "PARTICLE_QUAD_DEGENERATE", path + ".size",
                         "particle width and height must both exceed 1e-6", "particles", index,
                         "size");
            } else {
                particle.width = size.x;
                particle.height = size.y;
            }
        }
        numberField(entry, "orientation", path + ".orientation", particle.orientation, errors,
                    true, 0.0, "particles", index);
        numberField(entry, "transmittance", path + ".transmittance",
                    particle.transmittance, errors, true, 1.0, "particles", index);
        if (particle.transmittance < 0.0 || particle.transmittance > 1.0) {
            addError(errors, "PARTICLE_TRANSMITTANCE_OUT_OF_RANGE",
                     path + ".transmittance", "transmittance must be in [0, 1]", "particles",
                     index, "transmittance");
        }
        particle.color = Vec3{0.58, 0.60, 0.64};
        if (entry.get("color") != nullptr) {
            if (vec3Field(entry, "color", path + ".color", particle.color, errors, false,
                          Vec3{0.58, 0.60, 0.64}, "particles", index)) {
                if (particle.color.x < 0.0 || particle.color.x > 1.0 ||
                    particle.color.y < 0.0 || particle.color.y > 1.0 ||
                    particle.color.z < 0.0 || particle.color.z > 1.0) {
                    addError(errors, "PARTICLE_COLOR_OUT_OF_RANGE", path + ".color",
                             "particle color components must be in [0, 1]", "particles", index,
                             "color");
                }
            }
        }
        makeParticleBasis(particle);
        if (!isFinite(particle.center.x) || !isFinite(particle.center.y) ||
            !isFinite(particle.center.z) || !isFinite(particle.orientation)) {
            addError(errors, "PARTICLE_QUAD_DEGENERATE", path,
                     "particle geometry contains non-finite values", "particles", index, {});
        }
        scene.particles.push_back(particle);
    }
    if (scene.render.layerCount > 0) {
        for (int layer = 0; layer < scene.render.layerCount; ++layer) {
            if (!layerPresent[static_cast<size_t>(layer)]) {
                addError(errors, "PARTICLE_LAYER_MISSING", "particles[*].layer",
                         "declared layer " + std::to_string(layer) +
                             " has no particle",
                         "render.layers", layer, "layer");
            }
        }
    }
}

void validateLights(const Json& root, Scene& scene, std::vector<ValidationError>& errors) {
    const Json* lights = getArrayField(root, "lights", "lights", errors);
    if (lights == nullptr) {
        return;
    }
    if (lights->items().empty()) {
        addError(errors, "LIGHT_COLLECTION_EMPTY", "lights",
                 "at least one point light is required");
    }
    if (lights->items().size() > static_cast<size_t>(kMaxLights)) {
        addError(errors, "LIGHT_COUNT_LIMIT", "lights", "light count exceeds 64");
    }
    std::set<uint32_t> ids;
    for (size_t i = 0; i < lights->items().size(); ++i) {
        const Json& entry = lights->items()[i];
        const int index = static_cast<int>(i);
        const std::string path = indexPath("lights", index);
        if (!entry.isObject()) {
            addError(errors, "INVALID_TYPE", path, "light must be an object", "lights", index,
                     {});
            continue;
        }
        PointLight light;
        int64_t id = 0;
        if (integerField(entry, "id", path + ".id", id, errors, true, 0, "lights", index)) {
            if (id < 0 || static_cast<uint64_t>(id) > kMaxParticleId) {
                addError(errors, "LIGHT_ID_INVALID", path + ".id",
                         "light id must be in [0, 4294967294]", "lights", index, "id");
            } else {
                light.id = static_cast<uint32_t>(id);
                if (!ids.insert(light.id).second) {
                    addError(errors, "LIGHT_ID_DUPLICATE", path + ".id",
                             "light id must be unique", "lights", index, "id");
                }
            }
        }
        vec3Field(entry, "position", path + ".position", light.position, errors, true, {},
                  "lights", index);
        if (vec3Field(entry, "color", path + ".color", light.color, errors, true, {},
                      "lights", index)) {
            if (light.color.x < 0.0 || light.color.x > 1.0 || light.color.y < 0.0 ||
                light.color.y > 1.0 || light.color.z < 0.0 || light.color.z > 1.0) {
                addError(errors, "LIGHT_COLOR_OUT_OF_RANGE", path + ".color",
                         "light color components must be in [0, 1]", "lights", index, "color");
            }
        }
        if (numberField(entry, "intensity", path + ".intensity", light.intensity, errors, true,
                        0.0, "lights", index)) {
            if (light.intensity <= 0.0 || light.intensity > 1e9) {
                addError(errors, "LIGHT_INTENSITY_INVALID", path + ".intensity",
                         "intensity must be in (0, 1e9]", "lights", index, "intensity");
            }
        }
        if (numberField(entry, "radius", path + ".radius", light.radius, errors, false, 0.0,
                        "lights", index)) {
            if (light.radius < 0.0 || light.radius > 1e6) {
                addError(errors, "LIGHT_RADIUS_INVALID", path + ".radius",
                         "radius must be in [0, 1e6]", "lights", index, "radius");
            }
        }
        scene.lights.push_back(light);
    }
}

void validateCameras(const Json& root, Scene& scene,
                     std::vector<ValidationError>& errors) {
    const Json* cameras = getArrayField(root, "camera_path", "camera_path", errors);
    if (cameras == nullptr) {
        return;
    }
    if (cameras->items().empty()) {
        addError(errors, "CAMERA_PATH_EMPTY", "camera_path",
                 "camera path must contain at least one sample");
    }
    if (cameras->items().size() > static_cast<size_t>(kMaxCameraSamples)) {
        addError(errors, "CAMERA_PATH_TOO_LONG", "camera_path",
                 "camera path exceeds 100000 samples");
    }
    bool previousTimestampValid = false;
    double previousTimestamp = 0.0;
    for (size_t i = 0; i < cameras->items().size(); ++i) {
        const Json& entry = cameras->items()[i];
        const int index = static_cast<int>(i);
        const std::string path = indexPath("camera_path", index);
        if (!entry.isObject()) {
            addError(errors, "INVALID_TYPE", path, "camera sample must be an object",
                     "camera_path", index, {});
            previousTimestampValid = false;
            continue;
        }
        CameraSample sample;
        const bool timestampValid =
            numberField(entry, "timestamp", path + ".timestamp", sample.timestamp, errors,
                        true, 0.0, "camera_path", index);
        if (timestampValid) {
            if (previousTimestampValid && !(sample.timestamp > previousTimestamp)) {
                addError(errors, "CAMERA_TIMESTAMP_NOT_INCREASING", path + ".timestamp",
                         "timestamp must be strictly greater than the previous sample",
                         "camera_path", index, "timestamp");
            }
            previousTimestamp = sample.timestamp;
            previousTimestampValid = true;
        } else {
            previousTimestampValid = false;
        }
        vec3Field(entry, "position", path + ".position", sample.position, errors, true, {},
                  "camera_path", index);
        vec3Field(entry, "target", path + ".target", sample.target, errors, true, {},
                  "camera_path", index);
        vec3Field(entry, "up", path + ".up", sample.up, errors, true, {},
                  "camera_path", index);
        numberField(entry, "fov_degrees", path + ".fov_degrees", sample.fovDegrees, errors,
                    true, 45.0, "camera_path", index);
        if (sample.fovDegrees <= 0.0 || sample.fovDegrees >= 180.0) {
            addError(errors, "CAMERA_FOV_INVALID", path + ".fov_degrees",
                     "fov_degrees must be in (0, 180)", "camera_path", index,
                     "fov_degrees");
        }
        const Vec3 forward = sample.target - sample.position;
        if (length(forward) < 1e-8) {
            addError(errors, "CAMERA_VIEW_DEGENERATE", path + ".target",
                     "camera target must differ from position", "camera_path", index,
                     "target");
        }
        if (length(sample.up) < 1e-8 || length(cross(normalize(forward), sample.up)) < 1e-8) {
            addError(errors, "CAMERA_UP_DEGENERATE", path + ".up",
                     "up vector must be non-zero and not parallel to the view direction",
                     "camera_path", index, "up");
        }
        scene.cameras.push_back(sample);
    }
}

}  // namespace

bool loadScene(const std::filesystem::path& path, Scene& scene,
               std::vector<ValidationError>& errors) {
    scene = Scene{};
    std::string readError;
    if (!readFileBytes(path, scene.rawBytes, readError)) {
        addError(errors, "SCENE_READ_ERROR", "$", readError);
        return false;
    }
    scene.digest = Sha256::hash(scene.rawBytes);
    const std::string text(scene.rawBytes.begin(), scene.rawBytes.end());
    Json root;
    JsonParseError parseError;
    if (!parseJson(text, root, parseError)) {
        addError(errors, "JSON_PARSE_ERROR", "$",
                 "line " + std::to_string(parseError.line) + ", column " +
                     std::to_string(parseError.column) + ": " + parseError.message);
        return false;
    }
    if (!root.isObject()) {
        addError(errors, "SCENE_ROOT_NOT_OBJECT", "$", "scene root must be a JSON object");
        return false;
    }
    validateImage(root, scene, errors);
    validateRender(root, scene, errors);
    validateGround(root, scene, errors);
    validateParticles(root, scene, errors);
    validateLights(root, scene, errors);
    validateCameras(root, scene, errors);

    if (!errors.empty()) {
        return false;
    }
    if (scene.render.defaultFrame < 0 ||
        scene.render.defaultFrame >= static_cast<int>(scene.cameras.size())) {
        addError(errors, "CAMERA_FRAME_INDEX_OUT_OF_RANGE", "render.default_frame",
                 "default_frame is outside camera_path", "render", -1, "default_frame");
        return false;
    }
    if (!scene.render.probes.empty()) {
        std::set<uint32_t> lightIds;
        for (const auto& light : scene.lights) {
            lightIds.insert(light.id);
        }
        for (size_t i = 0; i < scene.render.probes.size(); ++i) {
            if (lightIds.find(scene.render.probes[i].lightId) == lightIds.end()) {
                addError(errors, "INDEX_OUT_OF_RANGE",
                         "render.shadow_probes[" + std::to_string(i) + "].light_id",
                         "probe light_id does not identify a point light",
                         "render.shadow_probes", static_cast<int>(i), "light_id");
            }
        }
    }
    if (!errors.empty()) {
        return false;
    }

    std::sort(scene.particles.begin(), scene.particles.end(),
              [](const Particle& a, const Particle& b) {
                  if (a.layer != b.layer) {
                      return a.layer < b.layer;
                  }
                  return a.id < b.id;
              });
    std::sort(scene.lights.begin(), scene.lights.end(),
              [](const PointLight& a, const PointLight& b) { return a.id < b.id; });
    return true;
}

bool validateSelectedFrame(const Scene& scene, int frame,
                           std::vector<ValidationError>& errors) {
    if (frame < 0 || frame >= static_cast<int>(scene.cameras.size())) {
        addError(errors, "CAMERA_FRAME_INDEX_OUT_OF_RANGE",
                 "camera_path[" + std::to_string(frame) + "]",
                 "selected camera index is outside camera_path", "camera_path", frame,
                 "index");
        return false;
    }
    (void)scene;
    return true;
}

int effectiveFrame(const Scene& scene, int overrideFrame) {
    return overrideFrame >= 0 ? overrideFrame : scene.render.defaultFrame;
}

const CameraSample& selectedCamera(const Scene& scene, int frame) {
    return scene.cameras[static_cast<size_t>(frame)];
}

Vec3 groundPoint(const Ground& ground, Vec2 uv) {
    return ground.center + ground.uAxis * uv.x + ground.vAxis * uv.y;
}

Vec2 groundCoordinates(const Ground& ground, Vec3 world, bool* onPlane) {
    const Vec3 delta = world - ground.center;
    if (onPlane != nullptr) {
        *onPlane = std::abs(dot(delta, ground.normal)) < 1e-6;
    }
    return {dot(delta, ground.uAxis), dot(delta, ground.vAxis)};
}

bool insideGround(const Ground& ground, Vec2 uv, double margin) {
    return uv.x >= -ground.halfU - margin && uv.x <= ground.halfU + margin &&
           uv.y >= -ground.halfV - margin && uv.y <= ground.halfV + margin;
}

Json makeErrorReport(const std::vector<ValidationError>& errors,
                     const std::string& source) {
    Json report = Json::object();
    report.set("status", Json(std::string("error")));
    report.set("report_version", Json(1.0));
    if (!source.empty()) {
        report.set("source", Json(source));
    }
    report.set("error_count", Json(static_cast<double>(errors.size())));
    Json list = Json::array();
    for (const auto& error : errors) {
        Json item = Json::object();
        item.set("code", Json(error.code));
        item.set("path", Json(error.path));
        item.set("message", Json(error.message));
        Json element = Json::object();
        if (!error.element.collection.empty()) {
            element.set("collection", Json(error.element.collection));
        }
        if (error.element.index >= 0) {
            element.set("index", Json(static_cast<double>(error.element.index)));
        }
        if (!error.element.field.empty()) {
            element.set("field", Json(error.element.field));
        }
        item.set("element", std::move(element));
        list.push(std::move(item));
    }
    report.set("errors", std::move(list));
    return report;
}

std::string makeDeterministicSceneJson() {
    Json root = Json::object();
    root.set("schema", Json(std::string("softraster-scene-v1")));

    Json image = Json::object();
    image.set("width", Json(480.0));
    image.set("height", Json(320.0));
    image.set("tile_width", Json(80.0));
    image.set("tile_height", Json(80.0));
    image.set("pixel_samples", Json(2.0));
    root.set("image", std::move(image));

    Json render = Json::object();
    render.set("layer_count", Json(22.0));
    render.set("default_frame", Json(0.0));
    render.set("shadow_samples", Json(9.0));
    render.set("softness_scale", Json(0.055));
    render.set("exposure", Json(1.35));
    render.set("ambient", Json::array().push(Json(0.012)).push(Json(0.014))
                              .push(Json(0.021)));
    root.set("render", std::move(render));

    Json ground = Json::object();
    ground.set("center", Json::array().push(Json(0.0)).push(Json(0.0)).push(Json(0.0)));
    ground.set("normal", Json::array().push(Json(0.0)).push(Json(1.0)).push(Json(0.0)));
    ground.set("half_extent", Json::array().push(Json(5.5)).push(Json(5.5)));
    ground.set("albedo", Json::array().push(Json(0.22)).push(Json(0.19))
                             .push(Json(0.15)));
    root.set("ground", std::move(ground));

    Json particles = Json::array();
    constexpr int layers = 22;
    constexpr int perLayer = 5;
    for (int layer = 0; layer < layers; ++layer) {
        const double height = 0.52 + 0.155 * static_cast<double>(layer);
        const double ring = 0.055 + 0.034 * static_cast<double>(layer);
        for (int j = 0; j < perLayer; ++j) {
            const double baseAngle =
                2.0 * kPi * static_cast<double>(j) / static_cast<double>(perLayer);
            const double angle =
                baseAngle + 0.31 * std::sin(1.7 * layer + 0.9 * j) + 0.08 * layer;
            const double radial =
                ring * (1.0 + 0.17 * std::sin(2.3 * layer + 1.1 * j));
            const double y =
                height + 0.035 * std::sin(1.3 * layer + 2.0 * j);
            const double width =
                (0.22 + 0.0065 * layer) * (0.92 + 0.11 * std::cos(0.8 * j + layer));
            const double particleHeight = 0.31 + 0.013 * layer;
            const double transmittance =
                0.885 + 0.020 * std::sin(0.37 * layer + 1.2 * j);
            const double fade = static_cast<double>(layer) / static_cast<double>(layers - 1);
            const Vec3 color{0.64 - 0.30 * fade, 0.60 - 0.28 * fade,
                             0.59 - 0.24 * fade};

            Json particle = Json::object();
            particle.set("id", Json(static_cast<double>(1000 + layer * 10 + j)));
            particle.set("layer", Json(static_cast<double>(layer)));
            particle.set("center",
                         Json::array().push(Json(radial * std::cos(angle)))
                             .push(Json(y))
                             .push(Json(radial * std::sin(angle))));
            particle.set("size",
                         Json::array().push(Json(width)).push(Json(particleHeight)));
            particle.set("orientation",
                         Json(angle + 0.45 * std::sin(0.63 * layer + 2.4 * j)));
            particle.set("transmittance", Json(transmittance));
            particle.set("color",
                         Json::array().push(Json(color.x)).push(Json(color.y))
                             .push(Json(color.z)));
            particles.push(std::move(particle));
        }
    }
    root.set("particles", std::move(particles));

    Json lights = Json::array();
    lights.push(Json::object()
                    .set("id", Json(1.0))
                    .set("position", Json::array().push(Json(0.0)).push(Json(1.65))
                                         .push(Json(0.0)))
                    .set("color", Json::array().push(Json(1.0)).push(Json(0.46))
                                      .push(Json(0.14)))
                    .set("intensity", Json(6.0))
                    .set("radius", Json(0.12)));
    lights.push(Json::object()
                    .set("id", Json(2.0))
                    .set("position", Json::array().push(Json(-0.55)).push(Json(2.55))
                                         .push(Json(0.45)))
                    .set("color", Json::array().push(Json(1.0)).push(Json(0.70))
                                      .push(Json(0.34)))
                    .set("intensity", Json(4.2))
                    .set("radius", Json(0.16)));
    root.set("lights", std::move(lights));

    Json cameras = Json::array();
    constexpr int cameraCount = 16;
    for (int i = 0; i < cameraCount; ++i) {
        const double angle = 2.0 * kPi * static_cast<double>(i) /
                             static_cast<double>(cameraCount);
        const double radius = 4.7 + 0.18 * std::cos(0.7 * i);
        const double cameraHeight = 1.55 + 0.12 * std::sin(0.45 * i);
        Json camera = Json::object();
        camera.set("timestamp", Json(0.5 * static_cast<double>(i)));
        camera.set("position",
                   Json::array().push(Json(radius * std::sin(angle)))
                       .push(Json(cameraHeight))
                       .push(Json(radius * std::cos(angle))));
        camera.set("target",
                   Json::array().push(Json(0.0)).push(Json(1.25)).push(Json(0.0)));
        camera.set("up", Json::array().push(Json(0.0)).push(Json(2.0)).push(Json(0.0)));
        camera.set("fov_degrees", Json(43.0));
        cameras.push(std::move(camera));
    }
    root.set("camera_path", std::move(cameras));
    return root.dump(2);
}

}  // namespace sr
