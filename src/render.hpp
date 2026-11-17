#pragma once

#include "common.hpp"
#include "scene.hpp"

#include <array>
#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace mosaic {

struct TextureBubble {
    double centerU = 0.0;
    double centerV = 0.0;
    double radius = 0.0;
    double strength = 0.0;
    double iorDelta = 0.0;
    Rgb tint{};
};

struct TextureFlow {
    double directionU = 0.0;
    double directionV = 0.0;
    double frequency = 0.0;
    double phase = 0.0;
    double amplitude = 0.0;
    double colorPhase = 0.0;
};

struct PieceTexture {
    Rgb transmissionScale{1.0, 1.0, 1.0};
    double iorScale = 1.0;
    std::vector<TextureBubble> bubbles;
    std::vector<TextureFlow> flows;
};

struct BoundaryEdge {
    std::uint32_t first = 0;
    std::uint32_t second = 0;
};

struct PreparedPiece {
    const Piece* piece = nullptr;
    Vec3 boundingMin{};
    Vec3 boundingMax{};
    Vec3 center{};
    Vec3 normal{0.0, 1.0, 0.0};
    double characteristicSize = 1.0;
    std::vector<BoundaryEdge> boundaryEdges;
    PieceTexture texture;
};

struct CameraFrame {
    Vec3 position{};
    Vec3 forward{};
    Vec3 right{};
    Vec3 up{};
    double tanHalfFov = 0.0;
    double aspect = 1.0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
};

struct TextureTable {
    std::vector<std::uint8_t> bytes;
    std::string digest;
};

struct PreparedScene {
    const Scene* scene = nullptr;
    CameraFrame camera;
    std::vector<PreparedPiece> pieces;
    std::vector<std::int32_t> layers;
    std::int32_t maximumLayer = -1;
    double floorTop = 0.0;
    TextureTable textureTable;
};

struct ProjectedPoint {
    double x = 0.0;
    double y = 0.0;
    double depth = 0.0;
    bool visible = false;
};

enum class CausticTargetKind : std::int32_t {
    Floor = 0,
    OuterPiece = 1,
};

struct CausticRecord {
    std::uint32_t sampleIndex = 0;
    std::int32_t sourceLayer = 0;
    std::uint32_t sourcePieceId = 0;
    std::uint32_t lightId = 0;
    Vec3 sourcePoint{};
    CausticTargetKind targetKind = CausticTargetKind::Floor;
    std::int32_t targetLayer = -1;
    std::uint32_t targetPieceId = 0;
    Vec3 targetPoint{};
    double thickness = 0.0;
    double layerDistance = 0.0;
    double penumbraSigmaWorld = 0.0;
    double penumbraSigmaPixels = 0.0;
    double screenX = -1.0;
    double screenY = -1.0;
    double peakBrightness = 0.0;
    bool outerGeometryHit = false;
};

struct CausticPixel {
    std::uint64_t r = 0;
    std::uint64_t g = 0;
    std::uint64_t b = 0;
};

struct PenumbraProbe {
    std::int32_t sourceLayer = 0;
    std::uint32_t sourcePieceId = 0;
    std::uint32_t lightId = 0;
    double thickness = 0.0;
    double layerDistance = 0.0;
    double penumbraWidthPixels = 0.0;
    double peakBrightness = 0.0;
    std::array<std::uint16_t, 4> profileGray{};
};

struct CausticResult {
    std::vector<CausticRecord> records;
    std::vector<CausticPixel> accum;
    std::vector<std::uint16_t> gray;
    std::vector<std::uint8_t> recordsBinary;
    std::vector<std::uint8_t> grayBinary;
    std::uint32_t stripeCount = 0;
    std::vector<PenumbraProbe> probes;
    std::string digest;
};

struct LayerStats {
    std::int32_t layerIndex = 0;
    std::uint64_t visibleFragments = 0;
    std::uint64_t transmissionEnergyMicro = 0;
    std::uint64_t reflectionEnergyMicro = 0;
    std::uint64_t transmissionLuminanceMicro = 0;
};

struct ChunkResult {
    std::uint32_t index = 0;
    std::uint32_t x = 0;
    std::uint32_t y = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    std::vector<std::uint8_t> rgba;
    std::vector<std::uint64_t> ownership;
    std::vector<LayerStats> layerStats;
};

PreparedScene prepareScene(const Scene& scene);

CausticResult renderCaustics(const PreparedScene& prepared);

ChunkResult renderChunk(
    const PreparedScene& prepared,
    const CausticResult& caustics,
    std::uint32_t chunkIndex);

std::uint32_t chunkCount(const Scene& scene);

void chunkBounds(
    const Scene& scene,
    std::uint32_t chunkIndex,
    std::uint32_t& x,
    std::uint32_t& y,
    std::uint32_t& width,
    std::uint32_t& height);

std::uint64_t ownershipKey(const Piece& piece);

constexpr std::uint64_t kFloorOwnerKey = 0ULL;
constexpr std::uint64_t kNoOwnerKey = std::numeric_limits<std::uint64_t>::max();

std::string causticRecordsJson(const PreparedScene& prepared, const CausticResult& caustics);

std::string ownershipLegendJson(const Scene& scene);

std::string layerStatsJson(const std::vector<LayerStats>& stats);

}  // namespace mosaic
