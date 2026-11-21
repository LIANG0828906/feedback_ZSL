#include "preview/error_model.hpp"

#include "preview/hash.hpp"

#include <algorithm>
#include <iomanip>
#include <limits>
#include <locale>
#include <sstream>

namespace preview {

namespace {

std::string hexNumber(std::uint64_t value) {
    return "\"0x" + hex64(value) + "\"";
}

}  // namespace

double estimateBoundsTransformError(const Bounds3& bounds,
                                    const Camera& camera) noexcept {
    const double scale = std::max(
        1.0, std::max(bounds.maxExtent(),
                      std::max(length(camera.position), length(bounds.center()))));
    return 32.0 * static_cast<double>(std::numeric_limits<float>::epsilon()) *
           scale;
}

std::string FrameErrorSummary::canonicalJson() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << std::setprecision(17);
    out << "{\"summary_version\":\"" << summaryVersion << "\""
        << ",\"snapshot_version\":" << snapshotVersion
        << ",\"pyramid_version\":" << pyramidVersion
        << ",\"load_content_hash\":" << hexNumber(loadContentHash)
        << ",\"mask_hash\":" << hexNumber(maskHash)
        << ",\"blocks\":{\"total\":" << totalBlocks
        << ",\"visible\":" << visibleBlocks
        << ",\"occluded\":" << occludedBlocks
        << ",\"uncertain\":" << uncertainBlocks
        << ",\"culled\":" << culledBlocks << "}"
        << ",\"layer_switches\":" << layerSwitches
        << ",\"critical_blocks\":" << criticalBlocks
        << ",\"error_contributions\":{\"bbox_transform_world\":"
        << bboxTransformErrorContribution
        << ",\"pyramid_quantization_world\":"
        << pyramidQuantizationErrorContribution
        << ",\"layer_switch_jump_world\":" << layerSwitchJumpContribution
        << ",\"frustum_boundary_world\":"
        << frustumBoundaryErrorContribution
        << ",\"occlusion_upper_bound_max_world\":"
        << occlusionErrorUpperBoundMax << "}"
        << ",\"mask_deviation_upper_bound_pixels\":"
        << maskDeviationUpperBoundPixels
        << "}";
    return out.str();
}

std::uint64_t FrameErrorSummary::summaryHash() const {
    const std::string canonical = canonicalJson();
    return fnv1a64(canonical.data(), canonical.size());
}

}  // namespace preview
