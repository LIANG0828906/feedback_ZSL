#pragma once

#include "font_atlas/font.hpp"
#include "font_atlas/raster.hpp"
#include "font_atlas/trace.hpp"
#include "font_atlas/types.hpp"

#include <functional>
#include <mutex>
#include <set>
#include <string>

namespace font_atlas {

using ProgressCallback = std::function<void(const ProgressEvent&)>;

class FontEngine {
public:
    explicit FontEngine(Font font, TraceLog* trace = nullptr);

    const Font& font() const { return font_; }
    bool isGlyphBusy(const std::string& glyph) const;

    Result<RasterResult> rasterize(const RasterRequest& request,
                                   const CancelCheck& cancelled = {},
                                   const ProgressCallback& progress = {});

    Result<QuantErrorSummary> compareQuantization(const std::string& glyph,
                                                  int oldStep, int newStep,
                                                  RasterOptions options,
                                                  const CancelCheck& cancelled = {});

private:
    Font font_;
    TraceLog* trace_ = nullptr;
    mutable std::mutex busyMutex_;
    std::set<std::string> busyGlyphs_;
};

}  // namespace font_atlas

