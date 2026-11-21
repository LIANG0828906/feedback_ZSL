#include "preview/depth_pyramid.hpp"

#include "preview/culling.hpp"
#include "preview/hash.hpp"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <limits>

namespace preview {

namespace {

constexpr double kDepthEpsilon = 1e-9;

std::uint64_t doubleBits(double value) {
    std::uint64_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "Unexpected double width");
    std::memcpy(&bits, &value, sizeof(bits));
    return bits;
}

double quantizedDepthMaxForSnapshot(const LoadSnapshot& snapshot,
                                    const Camera& camera) {
    double maxDepth = camera.nearPlane * 1.001;
    const Mat4 view = camera.viewMatrix();
    for (const LoadedBlockRef& reference : snapshot.blocks) {
        if (reference.block == nullptr || !reference.block->occluder) {
            continue;
        }
        for (const Vec3& corner : reference.block->bounds.corners()) {
            const Vec4 cameraPoint =
                view * Vec4{corner.x, corner.y, corner.z, 1.0};
            const double depth = -cameraPoint.z;
            if (depth > maxDepth) {
                maxDepth = depth;
            }
        }
    }
    return std::min(camera.farPlane, std::max(maxDepth, camera.nearPlane * 1.001));
}

int layerCountForSize(int width, int height, int maxLevels) {
    int levels = 1;
    int w = width;
    int h = height;
    while ((w > 1 || h > 1) && levels < maxLevels) {
        w = std::max(1, (w + 1) / 2);
        h = std::max(1, (h + 1) / 2);
        ++levels;
    }
    return levels;
}

struct ViewTriangleVertex {
    Vec3 cameraPoint;
    double viewDepth = 0.0;
};

std::vector<ViewTriangleVertex> clipDepthRange(
    const std::vector<ViewTriangleVertex>& input,
    double nearPlane,
    double farPlane) {
    auto clipPlane = [](const std::vector<ViewTriangleVertex>& polygon,
                        auto inside, auto interpolate) {
        std::vector<ViewTriangleVertex> output;
        if (polygon.empty()) {
            return output;
        }
        for (std::size_t i = 0; i < polygon.size(); ++i) {
            const ViewTriangleVertex& current = polygon[i];
            const ViewTriangleVertex& previous =
                polygon[(i + polygon.size() - 1) % polygon.size()];
            const bool currentInside = inside(current);
            const bool previousInside = inside(previous);
            if (currentInside != previousInside) {
                output.push_back(interpolate(previous, current));
            }
            if (currentInside) {
                output.push_back(current);
            }
        }
        return output;
    };

    auto nearClip = clipPlane(
        input,
        [nearPlane](const ViewTriangleVertex& vertex) {
            return -vertex.cameraPoint.z >= nearPlane;
        },
        [nearPlane](const ViewTriangleVertex& a,
                    const ViewTriangleVertex& b) {
            const double da = -a.cameraPoint.z;
            const double db = -b.cameraPoint.z;
            const double denominator = db - da;
            const double t = std::abs(denominator) <= 1e-15
                                 ? 0.0
                                 : (nearPlane - da) / denominator;
            ViewTriangleVertex result;
            result.cameraPoint = a.cameraPoint + (b.cameraPoint - a.cameraPoint) * t;
            result.cameraPoint.z = -nearPlane;
            result.viewDepth = nearPlane;
            return result;
        });

    return clipPlane(
        nearClip,
        [farPlane](const ViewTriangleVertex& vertex) {
            return -vertex.cameraPoint.z <= farPlane;
        },
        [farPlane](const ViewTriangleVertex& a,
                   const ViewTriangleVertex& b) {
            const double da = -a.cameraPoint.z;
            const double db = -b.cameraPoint.z;
            const double denominator = db - da;
            const double t = std::abs(denominator) <= 1e-15
                                 ? 0.0
                                 : (farPlane - da) / denominator;
            ViewTriangleVertex result;
            result.cameraPoint = a.cameraPoint + (b.cameraPoint - a.cameraPoint) * t;
            result.cameraPoint.z = -farPlane;
            result.viewDepth = farPlane;
            return result;
        });
}

double edgeFunction(double ax, double ay, double bx, double by, double px, double py) {
    return (px - ax) * (by - ay) - (py - ay) * (bx - ax);
}

}  // namespace

bool PyramidKey::operator==(const PyramidKey& other) const noexcept {
    if (snapshotVersion != other.snapshotVersion ||
        snapshotContentHash != other.snapshotContentHash ||
        nearPlane != other.nearPlane || farPlane != other.farPlane ||
        quantizedDepthMax != other.quantizedDepthMax ||
        baseStep != other.baseStep || width != other.width ||
        height != other.height || levels != other.levels ||
        quantizationBits != other.quantizationBits) {
        return false;
    }
    for (int i = 0; i < 16; ++i) {
        if (matrix[i] != other.matrix[i]) {
            return false;
        }
    }
    return true;
}

std::uint64_t PyramidKey::hash() const noexcept {
    HashBuilder hash;
    hash.addPod(snapshotVersion);
    hash.addPod(snapshotContentHash);
    for (double value : matrix) {
        hash.addPod(doubleBits(value));
    }
    hash.addPod(doubleBits(nearPlane));
    hash.addPod(doubleBits(farPlane));
    hash.addPod(doubleBits(quantizedDepthMax));
    hash.addPod(doubleBits(baseStep));
    hash.addPod(width);
    hash.addPod(height);
    hash.addPod(levels);
    hash.addPod(quantizationBits);
    return hash.value();
}

PyramidProbe DepthPyramid::probe(const ScreenBounds& screenBounds,
                                 int level) const noexcept {
    PyramidProbe result;
    if (layers.empty() || level < 0 ||
        level >= static_cast<int>(layers.size()) || !screenBounds.intersects) {
        return result;
    }
    result.level = level;
    const DepthPyramidLayer& layer = layers[static_cast<std::size_t>(level)];
    result.quantStep = layer.quantStep;
    if (layer.width <= 0 || layer.height <= 0 || key.width <= 0 ||
        key.height <= 0) {
        return result;
    }
    const int tile = 1 << level;
    result.x0 = std::max(0, screenBounds.x0 / tile);
    result.y0 = std::max(0, screenBounds.y0 / tile);
    result.x1 = std::min(layer.width - 1, screenBounds.x1 / tile);
    result.y1 = std::min(layer.height - 1, screenBounds.y1 / tile);
    if (result.x0 > result.x1 || result.y0 > result.y1) {
        return result;
    }

    std::uint32_t maxCode = 0;
    for (int y = result.y0; y <= result.y1; ++y) {
        const std::size_t row =
            static_cast<std::size_t>(y) * static_cast<std::size_t>(layer.width);
        for (int x = result.x0; x <= result.x1; ++x) {
            maxCode = std::max(maxCode,
                               layer.codes[row + static_cast<std::size_t>(x)]);
        }
    }
    result.occluderDepth =
        key.nearPlane + static_cast<double>(maxCode) * layer.quantStep;
    return result;
}

double DepthPyramid::maxQuantizationError() const noexcept {
    double maximum = 0.0;
    for (const DepthPyramidLayer& layer : layers) {
        maximum = std::max(maximum, layer.quantStep * 0.5);
    }
    return maximum;
}

std::uint64_t DepthPyramid::contentHash() const noexcept {
    HashBuilder hash;
    hash.addPod(version);
    hash.addPod(key.hash());
    hash.addPod(static_cast<std::uint64_t>(layers.size()));
    for (const DepthPyramidLayer& layer : layers) {
        hash.addPod(layer.level);
        hash.addPod(layer.width);
        hash.addPod(layer.height);
        hash.addPod(doubleBits(layer.quantStep));
        hash.addPod(static_cast<std::uint64_t>(layer.codes.size()));
        if (!layer.codes.empty()) {
            hash.addBytes(layer.codes.data(),
                          layer.codes.size() * sizeof(std::uint32_t));
        }
    }
    return hash.value();
}

PyramidKey makeDepthPyramidKey(const LoadSnapshot& snapshot,
                               const Camera& camera,
                               const DepthPyramidConfig& config) noexcept {
    PyramidKey key;
    key.snapshotVersion = snapshot.version;
    key.snapshotContentHash = snapshot.contentHash;
    const Mat4 view = camera.viewMatrix();
    for (int row = 0; row < 4; ++row) {
        for (int column = 0; column < 4; ++column) {
            key.matrix[static_cast<std::size_t>(row * 4 + column)] =
                view.at(row, column);
        }
    }
    key.nearPlane = camera.nearPlane;
    key.farPlane = camera.farPlane;
    key.quantizedDepthMax =
        quantizedDepthMaxForSnapshot(snapshot, camera);
    key.width = config.width;
    key.height = config.height;
    key.quantizationBits =
        static_cast<std::uint32_t>(std::max(1, std::min(30, config.quantizationBits)));
    const std::uint64_t bucketCount =
        (1ull << static_cast<unsigned>(key.quantizationBits)) - 1ull;
    const double depthSpan =
        std::max(camera.nearPlane * 1e-6,
                 key.quantizedDepthMax - camera.nearPlane);
    key.baseStep = depthSpan / static_cast<double>(bucketCount);
    const int maxLevels = std::max(1, std::min(20, config.maxLevels));
    key.levels = layerCountForSize(config.width, config.height, maxLevels);
    return key;
}

DepthPyramidBuildResult buildDepthPyramid(
    const LoadSnapshot& snapshot,
    const Camera& camera,
    const DepthPyramidConfig& config,
    std::uint64_t pyramidVersion,
    const std::atomic<bool>* cancelFlag) {
    DepthPyramidBuildResult result;
    if (config.width <= 0 || config.height <= 0 ||
        config.width > 16384 || config.height > 16384) {
        result.diagnostic = makeDiagnostic(StatusCode::E_INVALID_ARGUMENT,
                                           "pyramid.size",
                                           "invalid pyramid dimensions");
        return result;
    }

    auto pyramid = std::make_shared<DepthPyramid>();
    pyramid->version = pyramidVersion;
    pyramid->key = makeDepthPyramidKey(snapshot, camera, config);
    const std::uint64_t bucketCount =
        (1ull << static_cast<unsigned>(pyramid->key.quantizationBits)) - 1ull;
    const std::uint32_t maxCode = static_cast<std::uint32_t>(
        std::min<std::uint64_t>(bucketCount, 0xffffffffull));

    pyramid->layers.reserve(static_cast<std::size_t>(pyramid->key.levels));
    for (int level = 0; level < pyramid->key.levels; ++level) {
        if (cancelFlag != nullptr && cancelFlag->load()) {
            result.diagnostic = makeDiagnostic(
                StatusCode::E_CANCELLED, "pyramid.layers",
                "render cancelled while building depth pyramid");
            return result;
        }
        DepthPyramidLayer layer;
        layer.level = level;
        const int divisor = 1 << level;
        layer.width = std::max(1, (config.width + divisor - 1) / divisor);
        layer.height = std::max(1, (config.height + divisor - 1) / divisor);
        layer.quantStep =
            pyramid->key.baseStep * static_cast<double>(1u << level);
        layer.codes.assign(static_cast<std::size_t>(layer.width) *
                               static_cast<std::size_t>(layer.height),
                           maxCode);
        pyramid->layers.push_back(std::move(layer));
    }

    std::size_t occluderVisit = 0;
    for (const LoadedBlockRef& reference : snapshot.blocks) {
        const SceneBlock* block = reference.block;
        if (block == nullptr || !block->occluder || block->indices.empty()) {
            continue;
        }
        if ((++occluderVisit % config.cancellationCheckpointBlocks) == 0 &&
            cancelFlag != nullptr && cancelFlag->load()) {
            result.diagnostic = makeDiagnostic(
                StatusCode::E_CANCELLED, "pyramid.blocks",
                "render cancelled while rasterizing occluders");
            return result;
        }
        const FrustumResult frustum = conservativeFrustumCull(block->bounds, camera);
        if (frustum.classification == FrustumClassification::Outside) {
            continue;
        }
        const Mat4 view = camera.viewMatrix();
        for (std::size_t triangle = 0; triangle + 2 < block->indices.size();
             triangle += 3) {
            std::vector<ViewTriangleVertex> polygon;
            polygon.reserve(3);
            bool vertexValid = true;
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
                    vertexValid = false;
                    break;
                }
                polygon.push_back(ViewTriangleVertex{
                    {cameraPoint.x, cameraPoint.y, cameraPoint.z},
                    -cameraPoint.z});
            }
            if (!vertexValid) {
                continue;
            }
            const auto clipped = clipDepthRange(polygon, camera.nearPlane,
                                                camera.farPlane);
            if (clipped.size() < 3) {
                continue;
            }
            const Mat4 projection = camera.projectionMatrix();
            const double p00 = projection.at(0, 0);
            const double p11 = projection.at(1, 1);
            const double p22 = projection.at(2, 2);
            const double p23 = projection.at(2, 3);
            auto projectPoint = [&](const Vec3& point) {
                const double invW = 1.0 / (-point.z);
                return Vec3{(point.x * p00 * invW * 0.5 + 0.5) *
                                static_cast<double>(config.width),
                            (0.5 - point.y * p11 * invW * 0.5) *
                                static_cast<double>(config.height),
                            (p22 * point.z + p23) * invW};
            };

            const Vec3 projected0 = projectPoint(clipped[0].cameraPoint);
            for (std::size_t i = 1; i + 1 < clipped.size(); ++i) {
                const Vec3 projected1 = projectPoint(clipped[i].cameraPoint);
                const Vec3 projected2 = projectPoint(clipped[i + 1].cameraPoint);

                auto rasterProjected = [&](const Vec3& pa, const Vec3& pb,
                                           const Vec3& pc) {
                    double area = edgeFunction(pa.x, pa.y, pb.x, pb.y, pc.x, pc.y);
                    if (std::abs(area) <= 1e-15) {
                        return;
                    }
                    const double sign = area > 0.0 ? 1.0 : -1.0;
                    const int minX = std::max(
                        0, static_cast<int>(std::floor(
                               std::min(pa.x, std::min(pb.x, pc.x)) - 0.5)));
                    const int maxX = std::min(
                        config.width - 1,
                        static_cast<int>(std::ceil(
                            std::max(pa.x, std::max(pb.x, pc.x)) + 0.5)));
                    const int minY = std::max(
                        0, static_cast<int>(std::floor(
                               std::min(pa.y, std::min(pb.y, pc.y)) - 0.5)));
                    const int maxY = std::min(
                        config.height - 1,
                        static_cast<int>(std::ceil(
                            std::max(pa.y, std::max(pb.y, pc.y)) + 0.5)));
                    const double f = camera.farPlane;
                    const double n = camera.nearPlane;
                    const double farNear = f * n;
                    const double farMinusNear = f - n;
                    const double farPlusNear = f + n;
                    DepthPyramidLayer& base = pyramid->layers[0];
                    for (int y = minY; y <= maxY; ++y) {
                        const double py = static_cast<double>(y) + 0.5;
                        for (int x = minX; x <= maxX; ++x) {
                            const double px = static_cast<double>(x) + 0.5;
                            const double w0 = edgeFunction(pb.x, pb.y, pc.x, pc.y, px, py);
                            const double w1 = edgeFunction(pc.x, pc.y, pa.x, pa.y, px, py);
                            const double w2 = edgeFunction(pa.x, pa.y, pb.x, pb.y, px, py);
                            const double s0 = w0 * sign;
                            const double s1 = w1 * sign;
                            const double s2 = w2 * sign;
                            if (s0 < -1e-12 || s1 < -1e-12 || s2 < -1e-12) {
                                continue;
                            }
                            const double sum = w0 + w1 + w2;
                            if (std::abs(sum) <= 1e-18) {
                                continue;
                            }
                            const double ndcZ = (w0 * pa.z + w1 * pb.z + w2 * pc.z) / sum;
                            const double denominator = farPlusNear - farMinusNear * ndcZ;
                            if (std::abs(denominator) <= 1e-18) {
                                continue;
                            }
                            const double viewDepth = 2.0 * farNear / denominator;
                            if (!(viewDepth >= camera.nearPlane - kDepthEpsilon &&
                                  viewDepth <= camera.farPlane + kDepthEpsilon)) {
                                continue;
                            }
                            double codeValue =
                                std::round((viewDepth - pyramid->key.nearPlane) /
                                           pyramid->key.baseStep);
                            codeValue = std::max(
                                0.0, std::min(static_cast<double>(maxCode),
                                              codeValue));
                            const std::uint32_t code =
                                static_cast<std::uint32_t>(codeValue);
                            std::uint32_t& destination =
                                base.codes[static_cast<std::size_t>(y) *
                                               static_cast<std::size_t>(base.width) +
                                           static_cast<std::size_t>(x)];
                            destination = std::min(destination, code);
                        }
                    }
                };
                rasterProjected(projected0, projected1, projected2);
            }
        }
    }

    for (int level = 1; level < pyramid->key.levels; ++level) {
        if (cancelFlag != nullptr && cancelFlag->load()) {
            result.diagnostic = makeDiagnostic(
                StatusCode::E_CANCELLED, "pyramid.layers",
                "render cancelled while downsampling depth pyramid");
            return result;
        }
        const DepthPyramidLayer& child =
            pyramid->layers[static_cast<std::size_t>(level - 1)];
        DepthPyramidLayer& parent =
            pyramid->layers[static_cast<std::size_t>(level)];
        const std::uint32_t divisor = 2u;
        const std::uint32_t parentMaxCode = static_cast<std::uint32_t>(
            std::min<std::uint64_t>(bucketCount, 0xffffffffull));
        for (int y = 0; y < parent.height; ++y) {
            for (int x = 0; x < parent.width; ++x) {
                std::uint32_t childMax = 0;
                for (int dy = 0; dy < 2; ++dy) {
                    for (int dx = 0; dx < 2; ++dx) {
                        const int cx = x * 2 + dx;
                        const int cy = y * 2 + dy;
                        if (cx < child.width && cy < child.height) {
                            childMax = std::max(
                                childMax,
                                child.codes[static_cast<std::size_t>(cy) *
                                                static_cast<std::size_t>(child.width) +
                                            static_cast<std::size_t>(cx)]);
                        }
                    }
                }
                const std::uint32_t requantized = static_cast<std::uint32_t>(
                    std::min<std::uint64_t>(
                        parentMaxCode,
                        (static_cast<std::uint64_t>(childMax) + divisor - 1u) /
                            divisor));
                parent.codes[static_cast<std::size_t>(y) *
                                 static_cast<std::size_t>(parent.width) +
                             static_cast<std::size_t>(x)] = requantized;
            }
        }
    }

    result.pyramid = std::move(pyramid);
    result.diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    return result;
}

PyramidRebuildReport makeRebuildReport(
    const std::shared_ptr<const DepthPyramid>& previous,
    const DepthPyramid& current) {
    PyramidRebuildReport report;
    report.rebuilt = true;
    report.newVersion = current.version;
    report.newLevels = static_cast<int>(current.layers.size());
    report.newDepthMin = current.key.nearPlane;
    report.newDepthMax = current.key.quantizedDepthMax;
    report.newConfigHash = current.key.hash();
    report.maxQuantizationError = current.maxQuantizationError();
    if (previous) {
        report.oldVersion = previous->version;
        report.oldLevels = static_cast<int>(previous->layers.size());
        report.oldDepthMin = previous->key.nearPlane;
        report.oldDepthMax = previous->key.quantizedDepthMax;
        report.oldConfigHash = previous->key.hash();
        report.depthRangeDifference =
            std::abs(report.newDepthMin - report.oldDepthMin) +
            std::abs(report.newDepthMax - report.oldDepthMax);
        report.maxQuantizationError =
            std::max(previous->maxQuantizationError(),
                     current.maxQuantizationError());
    } else {
        report.depthRangeDifference = current.key.quantizedDepthMax -
                                      current.key.nearPlane;
    }
    return report;
}

}  // namespace preview
