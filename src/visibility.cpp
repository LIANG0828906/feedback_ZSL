#include "preview/visibility.hpp"

#include "preview/hash.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>
#include <unordered_map>
#include <vector>

namespace preview {

namespace {

struct CameraVertex {
    Vec3 point;
};

std::vector<CameraVertex> clipNearFar(const std::vector<CameraVertex>& input,
                                      double nearPlane,
                                      double farPlane) {
    auto clip = [](const std::vector<CameraVertex>& polygon, auto inside,
                   auto intersection) {
        std::vector<CameraVertex> output;
        if (polygon.empty()) {
            return output;
        }
        for (std::size_t i = 0; i < polygon.size(); ++i) {
            const CameraVertex& current = polygon[i];
            const CameraVertex& previous =
                polygon[(i + polygon.size() - 1) % polygon.size()];
            const bool currentInside = inside(current);
            const bool previousInside = inside(previous);
            if (currentInside != previousInside) {
                output.push_back(intersection(previous, current));
            }
            if (currentInside) {
                output.push_back(current);
            }
        }
        return output;
    };

    const auto nearClipped = clip(
        input,
        [nearPlane](const CameraVertex& vertex) {
            return -vertex.point.z >= nearPlane;
        },
        [nearPlane](const CameraVertex& a, const CameraVertex& b) {
            const double da = -a.point.z;
            const double db = -b.point.z;
            const double denominator = db - da;
            const double t = std::abs(denominator) <= 1e-15
                                 ? 0.0
                                 : (nearPlane - da) / denominator;
            CameraVertex result;
            result.point = a.point + (b.point - a.point) * t;
            result.point.z = -nearPlane;
            return result;
        });

    return clip(
        nearClipped,
        [farPlane](const CameraVertex& vertex) {
            return -vertex.point.z <= farPlane;
        },
        [farPlane](const CameraVertex& a, const CameraVertex& b) {
            const double da = -a.point.z;
            const double db = -b.point.z;
            const double denominator = db - da;
            const double t = std::abs(denominator) <= 1e-15
                                 ? 0.0
                                 : (farPlane - da) / denominator;
            CameraVertex result;
            result.point = a.point + (b.point - a.point) * t;
            result.point.z = -farPlane;
            return result;
        });
}

double edge(double ax, double ay, double bx, double by, double px, double py) {
    return (px - ax) * (by - ay) - (py - ay) * (bx - ax);
}

struct ProjectedVertex {
    Vec3 screen;  // x, y, NDC depth
};

void rasterizeTriangle(const Vec3& a,
                       const Vec3& b,
                       const Vec3& c,
                       std::uint32_t color,
                       int width,
                       int height,
                       double nearPlane,
                       double farPlane,
                       std::vector<std::uint32_t>* pixels,
                       std::vector<double>* depthBuffer) {
    double area = edge(a.x, a.y, b.x, b.y, c.x, c.y);
    if (std::abs(area) <= 1e-15) {
        return;
    }
    const double sign = area > 0.0 ? 1.0 : -1.0;
    const int minX = std::max(
        0, static_cast<int>(std::floor(std::min(a.x, std::min(b.x, c.x)) - 0.5)));
    const int maxX = std::min(
        width - 1,
        static_cast<int>(std::ceil(std::max(a.x, std::max(b.x, c.x)) + 0.5)));
    const int minY = std::max(
        0, static_cast<int>(std::floor(std::min(a.y, std::min(b.y, c.y)) - 0.5)));
    const int maxY = std::min(
        height - 1,
        static_cast<int>(std::ceil(std::max(a.y, std::max(b.y, c.y)) + 0.5)));
    if (minX > maxX || minY > maxY) {
        return;
    }

    const double farNear = farPlane * nearPlane;
    const double farMinusNear = farPlane - nearPlane;
    const double farPlusNear = farPlane + nearPlane;
    for (int y = minY; y <= maxY; ++y) {
        const double py = static_cast<double>(y) + 0.5;
        for (int x = minX; x <= maxX; ++x) {
            const double px = static_cast<double>(x) + 0.5;
            const double w0 = edge(b.x, b.y, c.x, c.y, px, py);
            const double w1 = edge(c.x, c.y, a.x, a.y, px, py);
            const double w2 = edge(a.x, a.y, b.x, b.y, px, py);
            if (w0 * sign < -1e-12 || w1 * sign < -1e-12 ||
                w2 * sign < -1e-12) {
                continue;
            }
            const double sum = w0 + w1 + w2;
            if (std::abs(sum) <= 1e-18) {
                continue;
            }
            const double ndcZ = (w0 * a.z + w1 * b.z + w2 * c.z) / sum;
            const double denominator = farPlusNear - farMinusNear * ndcZ;
            if (std::abs(denominator) <= 1e-18) {
                continue;
            }
            const double viewDepth = 2.0 * farNear / denominator;
            if (!(viewDepth >= nearPlane - 1e-9 &&
                  viewDepth <= farPlane + 1e-9)) {
                continue;
            }
            const std::size_t pixelIndex =
                static_cast<std::size_t>(y) * static_cast<std::size_t>(width) +
                static_cast<std::size_t>(x);
            if (viewDepth < (*depthBuffer)[pixelIndex]) {
                (*depthBuffer)[pixelIndex] = viewDepth;
                (*pixels)[pixelIndex] = color;
            }
        }
    }
}

}  // namespace

std::uint64_t VisibilityMask::nonEmptyPixels() const noexcept {
    std::uint64_t count = 0;
    for (std::uint32_t pixel : pixels) {
        if (pixel != 0u) {
            ++count;
        }
    }
    return count;
}

VisibilityMask composeVisibilityMask(
    const LoadSnapshot& snapshot,
    const Camera& camera,
    const DepthPyramid& pyramid,
    const std::vector<BlockVisibility>& decisions) {
    VisibilityMask mask;
    mask.width = camera.width;
    mask.height = camera.height;
    mask.snapshotVersion = snapshot.version;
    mask.pyramidVersion = pyramid.version;
    mask.loadContentHash = snapshot.contentHash;
    if (mask.width <= 0 || mask.height <= 0) {
        return mask;
    }
    mask.pixels.assign(static_cast<std::size_t>(mask.width) *
                           static_cast<std::size_t>(mask.height),
                       0u);
    std::vector<double> depthBuffer(mask.pixels.size(),
                                    std::numeric_limits<double>::infinity());
    std::unordered_map<BlockId, VisibilityDecision> decisionById;
    for (const BlockVisibility& decision : decisions) {
        decisionById.emplace(decision.id, decision.decision);
        if (decision.decision == VisibilityDecision::Visible) {
            mask.visibleBlocks.push_back(decision.id);
        } else if (decision.decision == VisibilityDecision::Uncertain) {
            mask.uncertainBlocks.push_back(decision.id);
        }
    }

    const Mat4 view = camera.viewMatrix();
    const Mat4 projection = camera.projectionMatrix();
    const double p00 = projection.at(0, 0);
    const double p11 = projection.at(1, 1);
    const double p22 = projection.at(2, 2);
    const double p23 = projection.at(2, 3);
    auto projectPoint = [&](const Vec3& point) {
        const double inverseW = 1.0 / (-point.z);
        return Vec3{(point.x * p00 * inverseW * 0.5 + 0.5) *
                        static_cast<double>(mask.width),
                    (0.5 - point.y * p11 * inverseW * 0.5) *
                        static_cast<double>(mask.height),
                    (p22 * point.z + p23) * inverseW};
    };

    for (const LoadedBlockRef& reference : snapshot.blocks) {
        const SceneBlock* block = reference.block;
        if (block == nullptr || block->indices.empty()) {
            continue;
        }
        const auto decisionIt = decisionById.find(block->id);
        if (decisionIt == decisionById.end() ||
            (decisionIt->second != VisibilityDecision::Visible &&
             decisionIt->second != VisibilityDecision::Uncertain)) {
            continue;
        }
        for (std::size_t triangle = 0; triangle + 2 < block->indices.size();
             triangle += 3) {
            std::vector<CameraVertex> polygon;
            polygon.reserve(3);
            bool valid = true;
            for (int corner = 0; corner < 3; ++corner) {
                const SceneVertex& vertex =
                    block->vertices[block->indices[triangle +
                                                    static_cast<std::size_t>(corner)]];
                const Vec4 cameraPoint =
                    view * Vec4{static_cast<double>(vertex.x),
                                static_cast<double>(vertex.y),
                                static_cast<double>(vertex.z), 1.0};
                if (!std::isfinite(cameraPoint.x) ||
                    !std::isfinite(cameraPoint.y) ||
                    !std::isfinite(cameraPoint.z)) {
                    valid = false;
                    break;
                }
                polygon.push_back(CameraVertex{
                    {cameraPoint.x, cameraPoint.y, cameraPoint.z}});
            }
            if (!valid) {
                continue;
            }
            const auto clipped =
                clipNearFar(polygon, camera.nearPlane, camera.farPlane);
            if (clipped.size() < 3) {
                continue;
            }
            const ProjectedVertex projected0{projectPoint(clipped[0].point)};
            for (std::size_t i = 1; i + 1 < clipped.size(); ++i) {
                const ProjectedVertex projected1{projectPoint(clipped[i].point)};
                const ProjectedVertex projected2{
                    projectPoint(clipped[i + 1].point)};
                const std::uint32_t color = block->id + 1u;
                rasterizeTriangle(projected0.screen, projected1.screen,
                                  projected2.screen, color, mask.width,
                                  mask.height, camera.nearPlane,
                                  camera.farPlane, &mask.pixels, &depthBuffer);
            }
        }
    }

    HashBuilder hash;
    hash.addPod(mask.width);
    hash.addPod(mask.height);
    if (!mask.pixels.empty()) {
        hash.addBytes(mask.pixels.data(),
                      mask.pixels.size() * sizeof(std::uint32_t));
    }
    mask.contentHash = hash.value();
    return mask;
}

StatusCode validateVisibilityMaskBinding(const VisibilityMask& mask,
                                         const LoadSnapshot& snapshot,
                                         const DepthPyramid& pyramid,
                                         Diagnostic* diagnostic) {
    if (mask.snapshotVersion != snapshot.version ||
        mask.loadContentHash != snapshot.contentHash) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(
                StatusCode::E_SNAPSHOT_STALE, "mask.snapshot_version",
                "visibility mask is bound to a different load snapshot");
        }
        return StatusCode::E_SNAPSHOT_STALE;
    }
    if (mask.pyramidVersion != pyramid.version) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(
                StatusCode::E_SNAPSHOT_STALE, "mask.pyramid_version",
                "visibility mask is bound to a different pyramid version");
        }
        return StatusCode::E_SNAPSHOT_STALE;
    }
    HashBuilder hash;
    hash.addPod(mask.width);
    hash.addPod(mask.height);
    if (!mask.pixels.empty()) {
        hash.addBytes(mask.pixels.data(),
                      mask.pixels.size() * sizeof(std::uint32_t));
    }
    if (hash.value() != mask.contentHash) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_SNAPSHOT_CORRUPT,
                                         "mask.content_hash",
                                         "visibility mask content hash mismatch");
        }
        return StatusCode::E_SNAPSHOT_CORRUPT;
    }
    if (diagnostic != nullptr) {
        *diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    }
    return StatusCode::E_OK;
}

}  // namespace preview
