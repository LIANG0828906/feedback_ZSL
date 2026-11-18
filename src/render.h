#pragma once

#include "json.h"
#include "math.h"
#include "output.h"
#include "scene.h"
#include "texture.h"

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <tuple>
#include <vector>

namespace cr {

constexpr int kReceiverOwnerCode = 0;
constexpr int kBackgroundOwnerCode = -1;
constexpr int kReceiverTargetKey = -1000000;

struct LayerEnergyStats {
    int layerIndex = -1;
    double reflected = 0.0;
    double transmitted = 0.0;
    long long samples = 0;

    json::Value toJson(int layerIndex) const;
};

struct BlockStats {
    long long rays = 0;
    std::vector<LayerEnergyStats> layers;

    json::Value toJson() const;
};

struct TileData {
    int index = -1;
    int x = 0;
    int y = 0;
    int width = 0;
    int height = 0;
    std::vector<std::uint8_t> rgba;
    std::vector<int> owners;
    BlockStats stats;

    std::string rgbaDigest() const;
    std::string ownerDigest() const;
    Vec3 averageColor() const;
    json::Value summaryJson() const;
};

struct ProjectionLanding {
    int lightIndex = 0;
    std::string lightId;
    std::string target;
    int targetLayer = -1;
    int occluderLayer = -1;
    std::string componentId;
    Vec3 origin{};
    Vec3 point{};
    Vec2 uv{};
    bool inBounds = false;
    bool treatedAsUnoccluded = false;
};

struct PenumbraComparison {
    int lightIndex = 0;
    std::string lightId;
    std::string target;
    int targetLayer = -1;
    int occluderLayer = -1;
    double receiverDistance = 0.0;
    double lightOccluderDistance = 0.0;
    double penumbraRadius = 0.0;
    double penumbraWidth = 0.0;
    double edgeWidth50 = 0.0;
    std::vector<double> sampleOffsets;
    std::vector<double> sampleBrightness;
};

class RenderContext {
public:
    Scene scene;
    TextureBank textures;
    std::map<int, OwnerInfo> ownerTable;
    std::vector<ProjectionLanding> landings;
    std::vector<PenumbraComparison> penumbraComparisons;
    std::string projectionDigest;

    RenderContext(Scene sceneIn);

    TileData renderTile(int blockIndex, int x, int y, int width, int height) const;
    json::Value projectionReport() const;
    std::string projectionCanonical() const;

private:
    struct ShadowMap;
    struct Impl;
    std::shared_ptr<Impl> impl_;
};

std::string linearToSrgbHex(Vec3 linear, double exposure);

}  // namespace cr

