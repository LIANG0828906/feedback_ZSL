#pragma once

#include "font_atlas/font.hpp"
#include "font_atlas/types.hpp"

#include <functional>
#include <vector>

namespace font_atlas {

struct PreparedGlyph {
    const NormalizedGlyph* glyph = nullptr;
    BoundsD bounds;
    std::vector<BoundsD> contourBounds;
};

struct GlyphPlacement {
    int width = 0;
    int height = 0;
    double scale = 1.0;
    double originDeviceX = 1.0;
    double originDeviceY = 1.0;
    double originFontX = 0.0;
    double originFontY = 0.0;
};

struct RawRegion {
    PixelRect rect;
    std::vector<std::uint8_t> coverage;
    std::uint64_t samples = 0;
};

using CancelCheck = std::function<bool()>;
using SampleCheckpoint = std::function<void(std::uint64_t sampledRows)>;

Result<PreparedGlyph> prepareGlyph(const Font& font, const NormalizedGlyph& glyph);
Result<GlyphPlacement> placeGlyph(const Font& font, const PreparedGlyph& glyph,
                                  const RasterOptions& options);
Result<RawRegion> rasterRegion(const PreparedGlyph& glyph, const GlyphPlacement& placement,
                               const RasterOptions& options, const PixelRect& rect,
                               const CancelCheck& cancelled = {},
                               const SampleCheckpoint& checkpoint = {});
Result<void> validateRasterOptions(const RasterOptions& options);

std::uint64_t sampleCountFor(const PixelRect& rect, const RasterOptions& options);
std::uint64_t estimatedTaskBytes(const PixelRect& rect, const RasterOptions& options,
                                 std::uint64_t sampleBatchSize);

std::uint8_t quantizeCoverage(std::uint32_t coveredSamples, std::uint32_t totalSamples,
                              int step);
std::uint8_t quantizeByte(std::uint8_t coverage, int step);
std::string hashCoverage(const std::vector<std::uint8_t>& bytes);

}  // namespace font_atlas

