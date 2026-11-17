#include "json.hpp"
#include "render.hpp"
#include "scene.hpp"
#include "sha256.hpp"

#include <cassert>
#include <iostream>
#include <string>

namespace {

const char* kScene = R"JSON({
  "format": "mosaic-scene-v1",
  "seed": 232,
  "camera": {
    "position": [0.0, 3.0, 3.0],
    "target": [0.0, 0.0, 0.0],
    "up": [0.0, 1.0, 0.0],
    "fovYDeg": 42.0,
    "near": 0.01,
    "far": 100.0
  },
  "floor": {
    "vertices": [[-2,0,-2],[2,0,-2],[2,0,2],[-2,0,2]],
    "triangles": [[0,1,2],[0,2,3]]
  },
  "pieces": [
    {
      "layerIndex": 0, "pieceId": 0,
      "transmission": [0.62,0.72,0.80],
      "thickness": 0.02, "edgeIor": 1.52,
      "vertices": [[-0.5,0.1,-0.5],[0.5,0.1,-0.5],[0.5,0.1,0.5],[-0.5,0.1,0.5]],
      "triangles": [[0,1,2],[0,2,3]]
    },
    {
      "layerIndex": 1, "pieceId": 0,
      "transmission": [0.64,0.74,0.82],
      "thickness": 0.026, "edgeIor": 1.55,
      "vertices": [[-0.5,0.22,-0.5],[0.5,0.22,-0.5],[0.5,0.22,0.5],[-0.5,0.22,0.5]],
      "triangles": [[0,1,2],[0,2,3]]
    }
  ],
  "lights": [
    {"id": 0, "position": [-1.0,2.0,1.0], "color": [1.0,0.88,0.72], "intensity": 2.0}
  ],
  "settings": {
    "image": {"width": 32, "height": 24, "chunkWidth": 16, "chunkHeight": 12},
    "texture": {"bubblesPerPiece": 4, "flowBandsPerPiece": 2},
    "caustic": {
      "samplesPerEdge": 4,
      "penumbraBase": 0.006,
      "penumbraThickness": 0.45,
      "penumbraLayerDistance": 0.08,
      "intensity": 0.2,
      "edgeBand": 0.12,
      "splatSupport": 2.5
    },
    "glare": {"reflectionScale": 0.7, "edgePower": 4.0},
    "compositionOrder": ["transmission", "reflection"]
  }
})JSON";

}  // namespace

int main() {
    assert(
        mosaic::Sha256::digest("abc") ==
        "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    const auto parsedJson = mosaic::parseJson(R"({"a":[1,2,3],"b":"x"})");
    assert(parsedJson.value.has_value());
    assert(parsedJson.value->find("a") != nullptr);

    const auto parsed = mosaic::parseScene(kScene);
    assert(parsed.errors.empty());
    assert(parsed.scene.has_value());
    const std::string digest = mosaic::sceneDigest(*parsed.scene);
    assert(digest.size() == 64);
    assert(digest == mosaic::sceneDigest(*parsed.scene));

    const mosaic::PreparedScene prepared = mosaic::prepareScene(*parsed.scene);
    assert(prepared.pieces.size() == 2);
    assert(!prepared.textureTable.bytes.empty());
    assert(prepared.textureTable.digest.size() == 64);
    assert(mosaic::chunkCount(*parsed.scene) == 4);

    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    mosaic::chunkBounds(*parsed.scene, 3, x, y, width, height);
    assert(x == 16 && y == 12 && width == 16 && height == 12);

    const std::string original = std::string(kScene);
    std::string invalid = original;
    const std::string needle = "\"triangles\": [[0,1,2],[0,2,3]]";
    const std::size_t position = invalid.find(needle);
    assert(position != std::string::npos);
    invalid.replace(position, needle.size(), "\"triangles\": [[0,1,9],[0,2,3]]");
    const auto invalidScene = mosaic::parseScene(invalid);
    assert(!invalidScene.errors.empty());
    assert(invalidScene.errors.front().code == "MOSAIC_MESH_INDEX_OUT_OF_RANGE");

    std::cout << "core tests passed\n";
    return 0;
}
