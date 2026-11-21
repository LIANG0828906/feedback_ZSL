#include "font_atlas/raster.hpp"

#include "font_atlas/sha256.hpp"

#include <algorithm>
#include <cmath>
#include <limits>
#include <string>

namespace font_atlas {
namespace {

Diagnostic rasterError(ErrorCode code, const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = code;
    diagnostic.message = message;
    return diagnostic;
}

bool pointInContour(const std::vector<PointD>& contour, double x, double y, int& winding) {
    if (contour.size() < 3) {
        return false;
    }
    for (std::size_t i = 0; i < contour.size(); ++i) {
        const PointD& a = contour[i];
        const PointD& b = contour[(i + 1) % contour.size()];
        if (a.y <= y) {
            if (b.y > y) {
                const double cross = (b.x - a.x) * (y - a.y) - (x - a.x) * (b.y - a.y);
                if (cross > 0.0) {
                    ++winding;
                }
            }
        } else if (b.y <= y) {
            const double cross = (b.x - a.x) * (y - a.y) - (x - a.x) * (b.y - a.y);
            if (cross < 0.0) {
                --winding;
            }
        }
    }
    return true;
}

bool isFilled(int winding, FillRule rule) {
    if (rule == FillRule::NonZero) {
        return winding != 0;
    }
    return (winding & 1) != 0;
}

}  // namespace

Result<PreparedGlyph> prepareGlyph(const Font& font, const NormalizedGlyph& glyph) {
    (void)font;
    PreparedGlyph prepared;
    prepared.glyph = &glyph;
    prepared.bounds = glyph.bounds;
    for (const auto& contour : glyph.contours) {
        BoundsD bounds{std::numeric_limits<double>::infinity(),
                       std::numeric_limits<double>::infinity(),
                       -std::numeric_limits<double>::infinity(),
                       -std::numeric_limits<double>::infinity()};
        for (const auto& point : contour) {
            bounds.x0 = std::min(bounds.x0, point.x);
            bounds.y0 = std::min(bounds.y0, point.y);
            bounds.x1 = std::max(bounds.x1, point.x);
            bounds.y1 = std::max(bounds.y1, point.y);
        }
        if (!std::isfinite(bounds.x0)) {
            bounds = {0.0, 0.0, 0.0, 0.0};
        }
        prepared.contourBounds.push_back(bounds);
    }
    return Result<PreparedGlyph>::success(std::move(prepared));
}

Result<GlyphPlacement> placeGlyph(const Font& font, const PreparedGlyph& glyph,
                                  const RasterOptions& options) {
    auto validation = validateRasterOptions(options);
    if (!validation.ok()) {
        return Result<GlyphPlacement>::failure(std::move(validation.error));
    }
    const double scale = static_cast<double>(options.targetPixelSize) / font.unitsPerEm;
    if (!std::isfinite(scale) || scale <= 0.0) {
        return Result<GlyphPlacement>::failure(
            rasterError(ErrorCode::InvalidArgument, "glyph scale must be positive"));
    }
    GlyphPlacement placement;
    placement.scale = scale;
    placement.originDeviceX = 1.0 + options.subpixelX;
    placement.originDeviceY = 1.0 + options.subpixelY;
    placement.originFontX = glyph.bounds.x0;
    placement.originFontY = glyph.bounds.y1;
    const double width = std::ceil(std::max(0.0, glyph.bounds.width()) * scale) + 3.0;
    const double height = std::ceil(std::max(0.0, glyph.bounds.height()) * scale) + 3.0;
    if (width > 8192.0 || height > 8192.0) {
        return Result<GlyphPlacement>::failure(
            rasterError(ErrorCode::InvalidArgument,
                        "target glyph bitmap exceeds the 8192 px safety limit"));
    }
    placement.width = std::max(1, static_cast<int>(width));
    placement.height = std::max(1, static_cast<int>(height));
    return Result<GlyphPlacement>::success(std::move(placement));
}

Result<void> validateRasterOptions(const RasterOptions& options) {
    if (options.samplesPerAxis <= 0 || options.samplesPerAxis > 16) {
        return Result<void>::failure(
            rasterError(ErrorCode::InvalidArgument, "samplesPerAxis must be in [1, 16]"));
    }
    if (options.targetPixelSize <= 0 || options.targetPixelSize > 8192) {
        return Result<void>::failure(
            rasterError(ErrorCode::InvalidArgument, "targetPixelSize must be in [1, 8192]"));
    }
    if (options.tileWidth <= 0 || options.tileHeight <= 0 || options.tileWidth > 8192 ||
        options.tileHeight > 8192) {
        return Result<void>::failure(
            rasterError(ErrorCode::InvalidArgument, "tile dimensions must be in [1, 8192]"));
    }
    if (!std::isfinite(options.subpixelX) || !std::isfinite(options.subpixelY) ||
        options.subpixelX < -1.0 || options.subpixelX > 1.0 || options.subpixelY < -1.0 ||
        options.subpixelY > 1.0) {
        return Result<void>::failure(
            rasterError(ErrorCode::InvalidArgument, "subpixel offsets must be in [-1, 1]"));
    }
    if (options.seamTolerance < 0 || options.seamTolerance > 255) {
        return Result<void>::failure(
            rasterError(ErrorCode::InvalidArgument, "seamTolerance must be in [0, 255]"));
    }
    if (options.quantizationStep <= 0 || options.quantizationStep > 255) {
        return Result<void>::failure(
            rasterError(ErrorCode::QuantInvalid,
                        "quantizationStep must be in [1, 255] for an 8-bit coverage buffer"));
    }
    return Result<void>::success();
}

std::uint64_t sampleCountFor(const PixelRect& rect, const RasterOptions& options) {
    const std::uint64_t samplesPerPixel =
        static_cast<std::uint64_t>(options.samplesPerAxis) * options.samplesPerAxis;
    return static_cast<std::uint64_t>(rect.area()) * samplesPerPixel;
}

std::uint64_t estimatedTaskBytes(const PixelRect& rect, const RasterOptions& options,
                                 std::uint64_t sampleBatchSize) {
    const std::uint64_t pixels = static_cast<std::uint64_t>(rect.area());
    const std::uint64_t samples = sampleCountFor(rect, options);
    const std::uint64_t coverage = pixels;
    const std::uint64_t scanline = static_cast<std::uint64_t>(rect.width) * 32u;
    const std::uint64_t batch = std::max<std::uint64_t>(1, sampleBatchSize);
    const std::uint64_t transient = std::min(samples, batch) * 12u + 256u;
    return coverage + scanline + transient;
}

std::uint8_t quantizeByte(std::uint8_t coverage, int step) {
    if (step <= 1) {
        return coverage;
    }
    if (coverage >= 255) {
        return 255;
    }
    const long quantized = std::lround(static_cast<double>(coverage) / step) * step;
    return static_cast<std::uint8_t>(std::max(0L, std::min(255L, quantized)));
}

std::uint8_t quantizeCoverage(std::uint32_t coveredSamples, std::uint32_t totalSamples,
                              int step) {
    if (totalSamples == 0) {
        return 0;
    }
    const std::uint64_t scaled = static_cast<std::uint64_t>(coveredSamples) * 255u;
    const std::uint32_t coverage =
        static_cast<std::uint32_t>((scaled + totalSamples / 2u) / totalSamples);
    return quantizeByte(static_cast<std::uint8_t>(std::min<std::uint32_t>(255u, coverage)),
                        step);
}

Result<RawRegion> rasterRegion(const PreparedGlyph& glyph, const GlyphPlacement& placement,
                               const RasterOptions& options, const PixelRect& rect,
                               const CancelCheck& cancelled,
                               const SampleCheckpoint& checkpoint) {
    auto validation = validateRasterOptions(options);
    if (!validation.ok()) {
        return Result<RawRegion>::failure(std::move(validation.error));
    }
    if (glyph.glyph == nullptr) {
        return Result<RawRegion>::failure(
            rasterError(ErrorCode::Internal, "prepared glyph has no source"));
    }
    if (rect.x < 0 || rect.y < 0 || rect.width <= 0 || rect.height <= 0 ||
        rect.right() > placement.width || rect.bottom() > placement.height) {
        return Result<RawRegion>::failure(
            rasterError(ErrorCode::Internal, "raster region lies outside glyph bitmap"));
    }

    RawRegion region;
    region.rect = rect;
    region.coverage.assign(static_cast<std::size_t>(rect.area()), 0);
    region.samples = sampleCountFor(rect, options);
    const int samplesPerAxis = options.samplesPerAxis;
    const std::uint32_t totalSamples =
        static_cast<std::uint32_t>(samplesPerAxis * samplesPerAxis);
    const double inverseScale = 1.0 / placement.scale;
    const auto& contours = glyph.glyph->contours;

    for (int localY = 0; localY < rect.height; ++localY) {
        if (cancelled && cancelled()) {
            return Result<RawRegion>::failure(
                rasterError(ErrorCode::Cancelled, "rasterization cancelled"));
        }
        const int pixelY = rect.y + localY;
        for (int localX = 0; localX < rect.width; ++localX) {
            const int pixelX = rect.x + localX;
            std::uint32_t covered = 0;
            for (int sy = 0; sy < samplesPerAxis; ++sy) {
                const double deviceY =
                    pixelY + (static_cast<double>(sy) + 0.5) / samplesPerAxis;
                const double fontY =
                    placement.originFontY -
                    (deviceY - placement.originDeviceY) * inverseScale;
                for (int sx = 0; sx < samplesPerAxis; ++sx) {
                    const double deviceX =
                        pixelX + (static_cast<double>(sx) + 0.5) / samplesPerAxis;
                    const double fontX =
                        placement.originFontX +
                        (deviceX - placement.originDeviceX) * inverseScale;
                    int winding = 0;
                    for (std::size_t contourIndex = 0; contourIndex < contours.size();
                         ++contourIndex) {
                        const BoundsD& bounds = glyph.contourBounds[contourIndex];
                        if (fontX < bounds.x0 || fontX > bounds.x1 || fontY < bounds.y0 ||
                            fontY > bounds.y1) {
                            continue;
                        }
                        pointInContour(contours[contourIndex], fontX, fontY, winding);
                    }
                    if (isFilled(winding, options.fillRule)) {
                        ++covered;
                    }
                }
            }
            region.coverage[static_cast<std::size_t>(localY) * rect.width + localX] =
                quantizeCoverage(covered, totalSamples, options.quantizationStep);
        }
        if (checkpoint && (localY % 16 == 15 || localY + 1 == rect.height)) {
            checkpoint(static_cast<std::uint64_t>(localY + 1));
        }
    }
    return Result<RawRegion>::success(std::move(region));
}

std::string hashCoverage(const std::vector<std::uint8_t>& bytes) {
    return Sha256::hex(bytes.data(), bytes.size());
}

}  // namespace font_atlas
