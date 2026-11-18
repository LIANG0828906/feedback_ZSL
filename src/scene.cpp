#include "scene.h"

#include "hash.h"

#include <algorithm>
#include <cmath>
#include <fstream>
#include <limits>
#include <set>
#include <sstream>

namespace cr {
namespace {

constexpr double kEpsilon = 1e-9;

void addWarning(std::vector<Warning>& warnings, std::string code, std::string message,
                int layer, std::string componentId, std::string kind) {
    warnings.push_back(
        {std::move(code), std::move(message), layer, std::move(componentId), std::move(kind)});
}

const json::Value* member(const json::Value& object, const char* key, std::vector<Warning>& warnings,
                          int layer, const std::string& componentId, const std::string& kind,
                          bool required = true) {
    const json::Value* value = object.find(key);
    if (value == nullptr && required) {
        addWarning(warnings, "missing_field",
                   std::string("missing required field '") + key + "'", layer, componentId, kind);
    }
    return value;
}

bool readNumber(const json::Value& object, const char* key, double& out,
                std::vector<Warning>& warnings, int layer, const std::string& componentId,
                const std::string& kind, bool required = true) {
    const json::Value* value = member(object, key, warnings, layer, componentId, kind, required);
    if (value == nullptr) return false;
    if (!value->isNumber() || !std::isfinite(value->asNumber())) {
        addWarning(warnings, "invalid_number",
                   std::string("field '") + key + "' must be a finite number", layer, componentId,
                   kind);
        return false;
    }
    out = value->asNumber();
    return true;
}

bool readInt(const json::Value& object, const char* key, int& out, std::vector<Warning>& warnings,
             int layer, const std::string& componentId, const std::string& kind,
             bool required = true) {
    double number = 0.0;
    if (!readNumber(object, key, number, warnings, layer, componentId, kind, required)) return false;
    if (std::floor(number) != number || number < static_cast<double>(std::numeric_limits<int>::min()) ||
        number > static_cast<double>(std::numeric_limits<int>::max())) {
        addWarning(warnings, "invalid_integer",
                   std::string("field '") + key + "' must be an integer", layer, componentId, kind);
        return false;
    }
    out = static_cast<int>(number);
    return true;
}

bool readVec3(const json::Value& object, const char* key, Vec3& out,
              std::vector<Warning>& warnings, int layer, const std::string& componentId,
              const std::string& kind, bool required = true) {
    const json::Value* value = member(object, key, warnings, layer, componentId, kind, required);
    if (value == nullptr) return false;
    if (!value->isArray() || value->asArray().size() != 3) {
        addWarning(warnings, "invalid_vector",
                   std::string("field '") + key + "' must contain three numbers", layer,
                   componentId, kind);
        return false;
    }
    Vec3 parsed{};
    for (std::size_t i = 0; i < 3; ++i) {
        const json::Value& item = value->asArray()[i];
        if (!item.isNumber() || !std::isfinite(item.asNumber())) {
            addWarning(warnings, "invalid_vector",
                       std::string("field '") + key + "' contains a non-finite component", layer,
                       componentId, kind);
            return false;
        }
        parsed[i] = item.asNumber();
    }
    out = parsed;
    return true;
}

bool isUnitColor(Vec3 color) {
    return std::isfinite(color.x) && std::isfinite(color.y) && std::isfinite(color.z) &&
           color.x >= 0.0 && color.x <= 1.0 && color.y >= 0.0 && color.y <= 1.0 &&
           color.z >= 0.0 && color.z <= 1.0;
}

std::string readString(const json::Value& object, const char* key, std::vector<Warning>& warnings,
                       int layer, const std::string& componentId, const std::string& kind,
                       bool required = true) {
    const json::Value* value = member(object, key, warnings, layer, componentId, kind, required);
    if (value == nullptr) return {};
    if (!value->isString()) {
        addWarning(warnings, "invalid_string",
                   std::string("field '") + key + "' must be a string", layer, componentId, kind);
        return {};
    }
    return value->asString();
}

void validateMesh(const Mesh& mesh, int layer, const std::string& componentId,
                  const std::string& kind, std::vector<Warning>& warnings) {
    if (mesh.vertices.empty()) {
        addWarning(warnings, "missing_component_geometry",
                   "component mesh has no vertices", layer, componentId, kind);
        return;
    }
    if (mesh.triangles.empty()) {
        addWarning(warnings, "missing_component_geometry",
                   "component mesh has no triangles", layer, componentId, kind);
        return;
    }
    for (std::size_t t = 0; t < mesh.triangles.size(); ++t) {
        const Triangle& tri = mesh.triangles[t];
        for (int index : tri.indices) {
            if (index < 0 || static_cast<std::size_t>(index) >= mesh.vertices.size()) {
                std::ostringstream message;
                message << "triangle " << t << " index " << index << " is outside vertex range [0,"
                        << mesh.vertices.size() << ")";
                addWarning(warnings, "mesh_index_out_of_range", message.str(), layer, componentId,
                           kind);
            }
        }
        if (tri.indices[0] < 0 || tri.indices[1] < 0 || tri.indices[2] < 0 ||
            static_cast<std::size_t>(tri.indices[0]) >= mesh.vertices.size() ||
            static_cast<std::size_t>(tri.indices[1]) >= mesh.vertices.size() ||
            static_cast<std::size_t>(tri.indices[2]) >= mesh.vertices.size()) {
            continue;
        }
        const Vec3& a = mesh.vertices[static_cast<std::size_t>(tri.indices[0])];
        const Vec3& b = mesh.vertices[static_cast<std::size_t>(tri.indices[1])];
        const Vec3& c = mesh.vertices[static_cast<std::size_t>(tri.indices[2])];
        const double area2 = length(cross(b - a, c - a));
        if (!(area2 > 1e-12) || !std::isfinite(area2)) {
            std::ostringstream message;
            message << "triangle " << t << " is degenerate (twice-area=" << area2 << ")";
            addWarning(warnings, "degenerate_triangle", message.str(), layer, componentId, kind);
        }
    }
}

Mesh parseMesh(const json::Value& owner, int layer, const std::string& componentId,
               const std::string& kind, std::vector<Warning>& warnings) {
    Mesh mesh;
    const json::Value* meshValue = member(owner, "mesh", warnings, layer, componentId, kind);
    if (meshValue == nullptr) return mesh;
    if (!meshValue->isObject()) {
        addWarning(warnings, "invalid_mesh", "field 'mesh' must be an object", layer, componentId,
                   kind);
        return mesh;
    }
    const json::Value* vertices = member(*meshValue, "vertices", warnings, layer, componentId, kind);
    if (vertices != nullptr) {
        if (!vertices->isArray()) {
            addWarning(warnings, "invalid_mesh", "field 'vertices' must be an array", layer,
                       componentId, kind);
        } else {
            mesh.vertices.reserve(vertices->asArray().size());
            for (std::size_t i = 0; i < vertices->asArray().size(); ++i) {
                const json::Value& item = vertices->asArray()[i];
                if (!item.isArray() || item.asArray().size() != 3) {
                    addWarning(warnings, "invalid_vertex",
                               "vertex " + std::to_string(i) + " must contain three numbers", layer,
                               componentId, kind);
                    mesh.vertices.push_back({});
                    continue;
                }
                Vec3 vertex{};
                bool ok = true;
                for (std::size_t axis = 0; axis < 3; ++axis) {
                    const json::Value& coordinate = item.asArray()[axis];
                    if (!coordinate.isNumber() || !std::isfinite(coordinate.asNumber())) {
                        ok = false;
                        break;
                    }
                    vertex[axis] = coordinate.asNumber();
                }
                if (!ok) {
                    addWarning(warnings, "invalid_vertex",
                               "vertex " + std::to_string(i) + " contains a non-finite component",
                               layer, componentId, kind);
                }
                mesh.vertices.push_back(vertex);
            }
        }
    }
    const json::Value* triangles =
        member(*meshValue, "triangles", warnings, layer, componentId, kind);
    if (triangles != nullptr) {
        if (!triangles->isArray()) {
            addWarning(warnings, "invalid_mesh", "field 'triangles' must be an array", layer,
                       componentId, kind);
        } else {
            mesh.triangles.reserve(triangles->asArray().size());
            for (std::size_t i = 0; i < triangles->asArray().size(); ++i) {
                const json::Value& item = triangles->asArray()[i];
                Triangle tri;
                bool triangleOk = true;
                if (!item.isObject()) {
                    addWarning(warnings, "invalid_triangle",
                               "triangle " + std::to_string(i) + " must be an object", layer,
                               componentId, kind);
                    triangleOk = false;
                } else {
                    const json::Value* indices = item.find("indices");
                    if (indices == nullptr || !indices->isArray() ||
                        indices->asArray().size() != 3) {
                        addWarning(warnings, "invalid_triangle",
                                   "triangle " + std::to_string(i) +
                                       " must have three indices",
                                   layer, componentId, kind);
                        triangleOk = false;
                    } else {
                        for (std::size_t k = 0; k < 3; ++k) {
                            const json::Value& indexValue = indices->asArray()[k];
                            if (!indexValue.isNumber() ||
                                std::floor(indexValue.asNumber()) != indexValue.asNumber() ||
                                indexValue.asNumber() < 0.0 ||
                                indexValue.asNumber() >
                                    static_cast<double>(std::numeric_limits<int>::max())) {
                                addWarning(warnings, "invalid_triangle",
                                           "triangle " + std::to_string(i) +
                                               " has a non-integer index",
                                           layer, componentId, kind);
                                triangleOk = false;
                            } else {
                                tri.indices[k] = static_cast<int>(indexValue.asNumber());
                            }
                        }
                    }
                    const json::Value* uv = item.find("uv");
                    if (uv != nullptr) {
                        if (!uv->isArray() || uv->asArray().size() != 3) {
                            addWarning(warnings, "invalid_uv",
                                       "triangle " + std::to_string(i) +
                                           " uv must contain three pairs",
                                       layer, componentId, kind);
                            triangleOk = false;
                        } else {
                            for (std::size_t k = 0; k < 3; ++k) {
                                const json::Value& pair = uv->asArray()[k];
                                if (!pair.isArray() || pair.asArray().size() != 2 ||
                                    !pair.asArray()[0].isNumber() ||
                                    !pair.asArray()[1].isNumber()) {
                                    addWarning(warnings, "invalid_uv",
                                               "triangle " + std::to_string(i) +
                                                   " has an invalid uv pair",
                                               layer, componentId, kind);
                                    triangleOk = false;
                                } else {
                                    tri.uv[k] = {pair.asArray()[0].asNumber(),
                                                 pair.asArray()[1].asNumber()};
                                }
                            }
                        }
                    } else {
                        tri.uv = {{{0.0, 0.0}, {1.0, 0.0}, {0.0, 1.0}}};
                    }
                }
                if (triangleOk) mesh.triangles.push_back(tri);
            }
        }
    }
    return mesh;
}

}  // namespace

json::Value Warning::toJson() const {
    json::Value result = json::Value::object();
    result.set("code", json::Value(code));
    result.set("message", json::Value(message));
    if (layer >= 0) result.set("layer", json::Value(layer));
    if (!componentId.empty()) result.set("componentId", json::Value(componentId));
    if (!kind.empty()) result.set("kind", json::Value(kind));
    return result;
}

json::Value warningsToJson(const std::vector<Warning>& warnings) {
    json::Value result = json::Value::object();
    result.set("format", json::Value("coffered-validation/v1"));
    result.set("ok", json::Value(warnings.empty()));
    result.set("warningCount", json::Value(static_cast<int>(warnings.size())));
    json::Value list = json::Value::array();
    for (const Warning& warning : warnings) list.push(warning.toJson());
    result.set("warnings", std::move(list));
    return result;
}

int Scene::blocksX() const {
    return render.tileSize > 0 ? (render.width + render.tileSize - 1) / render.tileSize : 0;
}

int Scene::blocksY() const {
    return render.tileSize > 0 ? (render.height + render.tileSize - 1) / render.tileSize : 0;
}

int Scene::totalBlocks() const {
    return blocksX() * blocksY();
}

LoadResult parseScene(const std::string& text, const std::string& sourceLabel) {
    LoadResult result;
    try {
        const json::Value root = json::parse(text);
        result.parsed = true;
        if (!root.isObject()) {
            result.fatalError = sourceLabel + ": root JSON value must be an object";
            return result;
        }
        Scene& scene = result.scene;
        scene.sourceCanonical = root.canonical();
        scene.digest = sha256Hex(scene.sourceCanonical);
        scene.schema = readString(root, "schema", scene.warnings, -1, "", "scene");
        scene.name = readString(root, "name", scene.warnings, -1, "", "scene", false);
        if (scene.schema.empty()) {
            addWarning(scene.warnings, "missing_schema", "scene schema must not be empty", -1, "",
                       "scene");
        }

        const json::Value* renderValue = member(root, "render", scene.warnings, -1, "", "scene");
        if (renderValue != nullptr && renderValue->isObject()) {
            const json::Value& render = *renderValue;
            readInt(render, "width", scene.render.width, scene.warnings, -1, "", "render");
            readInt(render, "height", scene.render.height, scene.warnings, -1, "", "render");
            readInt(render, "tile_size", scene.render.tileSize, scene.warnings, -1, "", "render");
            readInt(render, "texture_resolution", scene.render.textureResolution, scene.warnings,
                    -1, "", "render");
            readInt(render, "projection_samples", scene.render.projectionSamples, scene.warnings,
                    -1, "", "render");
            readInt(render, "shadow_map_resolution", scene.render.shadowMapResolution,
                    scene.warnings, -1, "", "render");
            readNumber(render, "penumbra_scale", scene.render.penumbraScale, scene.warnings, -1, "",
                       "render");
            readNumber(render, "exposure", scene.render.exposure, scene.warnings, -1, "", "render");
            readNumber(render, "ambient", scene.render.ambient, scene.warnings, -1, "", "render");
            readVec3(render, "background", scene.render.background, scene.warnings, -1, "", "render",
                     false);
            if (!isUnitColor(scene.render.background)) {
                addWarning(scene.warnings, "invalid_color",
                           "render.background components must be in [0,1]", -1, "", "render");
            }
        } else if (renderValue != nullptr) {
            addWarning(scene.warnings, "invalid_object", "field 'render' must be an object", -1, "",
                       "render");
        }
        RenderConfig& rc = scene.render;
        if (rc.width <= 0 || rc.width > 16384)
            addWarning(scene.warnings, "invalid_render_config", "render.width out of range", -1, "",
                       "render");
        if (rc.height <= 0 || rc.height > 16384)
            addWarning(scene.warnings, "invalid_render_config", "render.height out of range", -1, "",
                       "render");
        if (rc.tileSize <= 0 || rc.tileSize > 4096)
            addWarning(scene.warnings, "invalid_render_config", "render.tile_size out of range", -1,
                       "", "render");
        if (rc.textureResolution < 8 || rc.textureResolution > 2048)
            addWarning(scene.warnings, "invalid_render_config",
                       "render.texture_resolution out of range [8,2048]", -1, "", "render");
        if (rc.projectionSamples < 1 || rc.projectionSamples > 32)
            addWarning(scene.warnings, "invalid_render_config",
                       "render.projection_samples out of range [1,32]", -1, "", "render");
        if (rc.shadowMapResolution < 32 || rc.shadowMapResolution > 4096)
            addWarning(scene.warnings, "invalid_render_config",
                       "render.shadow_map_resolution out of range [32,4096]", -1, "", "render");
        if (!(rc.penumbraScale > 0.0) || !std::isfinite(rc.penumbraScale))
            addWarning(scene.warnings, "invalid_render_config", "render.penumbra_scale must be > 0",
                       -1, "", "render");
        if (!(rc.exposure > 0.0) || !std::isfinite(rc.exposure))
            addWarning(scene.warnings, "invalid_render_config", "render.exposure must be > 0", -1,
                       "", "render");
        if (rc.ambient < 0.0 || !std::isfinite(rc.ambient))
            addWarning(scene.warnings, "invalid_render_config",
                       "render.ambient must be a finite non-negative number", -1, "", "render");

        const json::Value* layersValue = member(root, "layers", scene.warnings, -1, "", "scene");
        std::set<int> layerIndices;
        std::set<std::string> componentIds;
        int ownerCode = 1;
        if (layersValue != nullptr) {
            if (!layersValue->isArray() || layersValue->asArray().empty()) {
                addWarning(scene.warnings, "missing_components",
                           "field 'layers' must be a non-empty array", -1, "<missing>", "layer");
            } else {
                for (std::size_t li = 0; li < layersValue->asArray().size(); ++li) {
                    const json::Value& layerValue = layersValue->asArray()[li];
                    if (!layerValue.isObject()) {
                        addWarning(scene.warnings, "invalid_layer",
                                   "layer " + std::to_string(li) + " must be an object", -1,
                                   "<missing>", "layer");
                        continue;
                    }
                    Layer layer;
                    readInt(layerValue, "layer", layer.index, scene.warnings, -1, "", "layer");
                    readNumber(layerValue, "z", layer.z, scene.warnings, layer.index, "", "layer");
                    readNumber(layerValue, "thickness", layer.thickness, scene.warnings, layer.index,
                               "", "layer");
                    if (layer.index < 0) {
                        addWarning(scene.warnings, "invalid_layer_index",
                                   "layer index must be non-negative", layer.index, "", "layer");
                    } else if (!layerIndices.insert(layer.index).second) {
                        addWarning(scene.warnings, "duplicate_layer",
                                   "duplicate layer index " + std::to_string(layer.index),
                                   layer.index, "", "layer");
                    }
                    if (!(layer.thickness > 0.0) || !std::isfinite(layer.thickness)) {
                        addWarning(scene.warnings, "invalid_layer_thickness",
                                   "layer thickness must be > 0", layer.index, "", "layer");
                    }

                    const json::Value* boardsValue = layerValue.find("boards");
                    if (boardsValue == nullptr || !boardsValue->isArray() ||
                        boardsValue->asArray().empty()) {
                        addWarning(scene.warnings, "missing_components",
                                   "layer has no board component", layer.index, "<missing>",
                                   "board");
                    } else {
                        for (std::size_t bi = 0; bi < boardsValue->asArray().size(); ++bi) {
                            const json::Value& boardValue = boardsValue->asArray()[bi];
                            if (!boardValue.isObject()) {
                                addWarning(scene.warnings, "invalid_component",
                                           "board component must be an object", layer.index,
                                           "<missing>", "board");
                                continue;
                            }
                            Board board;
                            board.layer = layer.index;
                            board.id = readString(boardValue, "id", scene.warnings, layer.index, "",
                                                  "board");
                            if (board.id.empty()) {
                                addWarning(scene.warnings, "missing_component_id",
                                           "board component is missing a non-empty id", layer.index,
                                           "<missing>", "board");
                            } else if (!componentIds.insert(board.id).second) {
                                addWarning(scene.warnings, "duplicate_component_id",
                                           "duplicate component id '" + board.id + "'", layer.index,
                                           board.id, "board");
                            }
                            double seed = 0.0;
                            if (readNumber(boardValue, "seed", seed, scene.warnings, layer.index,
                                           board.id, "board")) {
                                if (std::floor(seed) != seed || seed < 0.0 ||
                                    seed > 4294967295.0) {
                                    addWarning(scene.warnings, "invalid_seed",
                                               "board seed must be an unsigned 32-bit integer",
                                               layer.index, board.id, "board");
                                } else {
                                    board.seed = static_cast<std::uint32_t>(seed);
                                }
                            }
                            readNumber(boardValue, "transmittance", board.transmittance,
                                       scene.warnings, layer.index, board.id, "board");
                            if (board.transmittance < 0.0 || board.transmittance > 1.0 ||
                                !std::isfinite(board.transmittance)) {
                                addWarning(scene.warnings, "transmittance_out_of_range",
                                           "board transmittance must be in [0,1]", layer.index,
                                           board.id, "board");
                            }
                            readVec3(boardValue, "color", board.color, scene.warnings, layer.index,
                                     board.id, "board", false);
                            if (!isUnitColor(board.color)) {
                                addWarning(scene.warnings, "invalid_color",
                                           "board color components must be in [0,1]",
                                           layer.index, board.id, "board");
                            }
                            board.mesh = parseMesh(boardValue, layer.index, board.id, "board",
                                                   scene.warnings);
                            validateMesh(board.mesh, layer.index, board.id, "board", scene.warnings);
                            board.ownerCode = ownerCode++;
                            layer.boards.push_back(std::move(board));
                        }
                    }

                    const json::Value* bracketsValue = layerValue.find("brackets");
                    if (bracketsValue == nullptr || !bracketsValue->isArray() ||
                        bracketsValue->asArray().empty()) {
                        addWarning(scene.warnings, "missing_components",
                                   "layer has no bracket component", layer.index, "<missing>",
                                   "bracket");
                    } else {
                        for (std::size_t ki = 0; ki < bracketsValue->asArray().size(); ++ki) {
                            const json::Value& bracketValue = bracketsValue->asArray()[ki];
                            if (!bracketValue.isObject()) {
                                addWarning(scene.warnings, "invalid_component",
                                           "bracket component must be an object", layer.index,
                                           "<missing>", "bracket");
                                continue;
                            }
                            Bracket bracket;
                            bracket.layer = layer.index;
                            bracket.id = readString(bracketValue, "id", scene.warnings, layer.index,
                                                    "", "bracket");
                            if (bracket.id.empty()) {
                                addWarning(scene.warnings, "missing_component_id",
                                           "bracket component is missing a non-empty id",
                                           layer.index, "<missing>", "bracket");
                            } else if (!componentIds.insert(bracket.id).second) {
                                addWarning(scene.warnings, "duplicate_component_id",
                                           "duplicate component id '" + bracket.id + "'",
                                           layer.index, bracket.id, "bracket");
                            }
                            readNumber(bracketValue, "reflectance", bracket.reflectance,
                                       scene.warnings, layer.index, bracket.id, "bracket");
                            if (bracket.reflectance < 0.0 || bracket.reflectance > 1.0 ||
                                !std::isfinite(bracket.reflectance)) {
                                addWarning(scene.warnings, "reflectance_out_of_range",
                                           "bracket reflectance must be in [0,1]", layer.index,
                                           bracket.id, "bracket");
                            }
                            readNumber(bracketValue, "thickness", bracket.thickness, scene.warnings,
                                       layer.index, bracket.id, "bracket");
                            if (!(bracket.thickness > 0.0) || !std::isfinite(bracket.thickness)) {
                                addWarning(scene.warnings, "invalid_thickness",
                                           "bracket thickness must be > 0", layer.index,
                                           bracket.id, "bracket");
                            }
                            readVec3(bracketValue, "color", bracket.color, scene.warnings,
                                     layer.index, bracket.id, "bracket", false);
                            if (!isUnitColor(bracket.color)) {
                                addWarning(scene.warnings, "invalid_color",
                                           "bracket color components must be in [0,1]",
                                           layer.index, bracket.id, "bracket");
                            }
                            bracket.mesh = parseMesh(bracketValue, layer.index, bracket.id,
                                                     "bracket", scene.warnings);
                            validateMesh(bracket.mesh, layer.index, bracket.id, "bracket",
                                         scene.warnings);
                            bracket.ownerCode = ownerCode++;
                            layer.brackets.push_back(std::move(bracket));
                        }
                    }
                    scene.layers.push_back(std::move(layer));
                }
            }
        }
        std::sort(scene.layers.begin(), scene.layers.end(),
                  [](const Layer& a, const Layer& b) { return a.index < b.index; });

        const json::Value* lightsValue = member(root, "lights", scene.warnings, -1, "", "scene");
        if (lightsValue != nullptr) {
            if (!lightsValue->isArray() || lightsValue->asArray().empty()) {
                addWarning(scene.warnings, "missing_lights",
                           "field 'lights' must be a non-empty array", -1, "<missing>", "light");
            } else {
                std::set<std::string> lightIds;
                for (std::size_t i = 0; i < lightsValue->asArray().size(); ++i) {
                    const json::Value& lightValue = lightsValue->asArray()[i];
                    if (!lightValue.isObject()) {
                        addWarning(scene.warnings, "invalid_light", "light must be an object", -1,
                                   "<missing>", "light");
                        continue;
                    }
                    Light light;
                    light.id = readString(lightValue, "id", scene.warnings, -1, "", "light");
                    if (light.id.empty()) {
                        addWarning(scene.warnings, "missing_light_id",
                                   "light is missing a non-empty id", -1, "<missing>", "light");
                    } else if (!lightIds.insert(light.id).second) {
                        addWarning(scene.warnings, "duplicate_light_id",
                                   "duplicate light id '" + light.id + "'", -1, light.id, "light");
                    }
                    readVec3(lightValue, "position", light.position, scene.warnings, -1, light.id,
                             "light");
                    readVec3(lightValue, "color", light.color, scene.warnings, -1, light.id,
                             "light", false);
                    if (!isUnitColor(light.color)) {
                        addWarning(scene.warnings, "invalid_light_color",
                                   "light color components must be in [0,1]", -1, light.id,
                                   "light");
                    }
                    readNumber(lightValue, "intensity", light.intensity, scene.warnings, -1,
                               light.id, "light");
                    if (!(light.intensity > 0.0) || !std::isfinite(light.intensity)) {
                        addWarning(scene.warnings, "invalid_light_intensity",
                                   "light intensity must be > 0", -1, light.id, "light");
                    }
                    for (const Layer& layer : scene.layers) {
                        if (std::abs(light.position.z - layer.z) <= 1e-6) {
                            addWarning(scene.warnings, "light_on_layer_plane",
                                       "light lies in layer " + std::to_string(layer.index) +
                                           " plane z=" + json::numberToString(layer.z),
                                       layer.index, light.id, "light");
                        }
                    }
                    if (!componentIds.count(light.id)) {
                        componentIds.insert(light.id);
                    }
                    scene.lights.push_back(std::move(light));
                }
            }
        }

        const json::Value* receiverValue = member(root, "receiver", scene.warnings, -1, "", "scene");
        if (receiverValue != nullptr && receiverValue->isObject()) {
            const json::Value& receiver = *receiverValue;
            readNumber(receiver, "z", scene.receiver.z, scene.warnings, -1, "", "receiver");
            readNumber(receiver, "min_x", scene.receiver.minX, scene.warnings, -1, "", "receiver");
            readNumber(receiver, "max_x", scene.receiver.maxX, scene.warnings, -1, "", "receiver");
            readNumber(receiver, "min_y", scene.receiver.minY, scene.warnings, -1, "", "receiver");
            readNumber(receiver, "max_y", scene.receiver.maxY, scene.warnings, -1, "", "receiver");
            readVec3(receiver, "color", scene.receiver.color, scene.warnings, -1, "", "receiver",
                     false);
            if (!isUnitColor(scene.receiver.color)) {
                addWarning(scene.warnings, "invalid_color",
                           "receiver color components must be in [0,1]", -1, "", "receiver");
            }
            if (!(scene.receiver.minX < scene.receiver.maxX) ||
                !(scene.receiver.minY < scene.receiver.maxY)) {
                addWarning(scene.warnings, "invalid_receiver",
                           "receiver bounds must have min < max", -1, "", "receiver");
            }
        } else if (receiverValue != nullptr) {
            addWarning(scene.warnings, "invalid_receiver", "field 'receiver' must be an object",
                       -1, "", "receiver");
        }

        const json::Value* cameraValue = member(root, "camera", scene.warnings, -1, "", "scene");
        if (cameraValue != nullptr && cameraValue->isObject()) {
            const json::Value& camera = *cameraValue;
            readVec3(camera, "position", scene.camera.position, scene.warnings, -1, "", "camera");
            readVec3(camera, "target", scene.camera.target, scene.warnings, -1, "", "camera");
            readVec3(camera, "up", scene.camera.up, scene.warnings, -1, "", "camera");
            readNumber(camera, "fov_deg", scene.camera.fovDeg, scene.warnings, -1, "", "camera");
            readNumber(camera, "near", scene.camera.nearZ, scene.warnings, -1, "", "camera");
            readNumber(camera, "far", scene.camera.farZ, scene.warnings, -1, "", "camera");
            const Vec3 forward = normalize(scene.camera.target - scene.camera.position);
            const Vec3 up = normalize(scene.camera.up);
            if (length(forward) < 1e-12) {
                addWarning(scene.warnings, "invalid_camera",
                           "camera position and target must differ", -1, "", "camera");
            }
            if (length(up) < 1e-12 || std::abs(dot(forward, up)) > 0.999999) {
                addWarning(scene.warnings, "invalid_camera",
                           "camera up vector must be non-zero and not parallel to view direction",
                           -1, "", "camera");
            }
            if (!(scene.camera.fovDeg > 0.0) || !(scene.camera.fovDeg < 180.0)) {
                addWarning(scene.warnings, "invalid_camera", "camera fov_deg must be in (0,180)",
                           -1, "", "camera");
            }
            if (!(scene.camera.nearZ > 0.0) || !(scene.camera.farZ > scene.camera.nearZ)) {
                addWarning(scene.warnings, "invalid_camera",
                           "camera requires 0 < near < far", -1, "", "camera");
            }
        } else if (cameraValue != nullptr) {
            addWarning(scene.warnings, "invalid_camera", "field 'camera' must be an object", -1, "",
                       "camera");
        }
    } catch (const json::Error& error) {
        result.parsed = false;
        result.fatalError = sourceLabel + ": JSON parse error: " + error.what();
    } catch (const std::exception& error) {
        result.parsed = false;
        result.fatalError = sourceLabel + ": " + error.what();
    }
    return result;
}

LoadResult loadScene(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        LoadResult result;
        result.fatalError = "cannot open scene file: " + path;
        return result;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    return parseScene(buffer.str(), path);
}

}  // namespace cr

