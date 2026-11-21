#include "chunkpipe/visibility.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <iomanip>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <unordered_map>

#include "chunkpipe/hash.hpp"

namespace chunkpipe {

namespace {

constexpr float kEpsilon = 1e-6f;

struct Vec4 {
    float x = 0.0f;
    float y = 0.0f;
    float z = 0.0f;
    float w = 0.0f;
};

struct Mat4 {
    float m[16]{};
};

Mat4 multiply(const Mat4& a, const Mat4& b) {
    Mat4 result;
    for (int row = 0; row < 4; ++row) {
        for (int column = 0; column < 4; ++column) {
            float value = 0.0f;
            for (int index = 0; index < 4; ++index) {
                value += a.m[row * 4 + index] * b.m[index * 4 + column];
            }
            result.m[row * 4 + column] = value;
        }
    }
    return result;
}

Vec4 transform(const Mat4& matrix, const Vec3& point) {
    return {matrix.m[0] * point.x + matrix.m[1] * point.y + matrix.m[2] * point.z +
                matrix.m[3],
            matrix.m[4] * point.x + matrix.m[5] * point.y + matrix.m[6] * point.z +
                matrix.m[7],
            matrix.m[8] * point.x + matrix.m[9] * point.y + matrix.m[10] * point.z +
                matrix.m[11],
            matrix.m[12] * point.x + matrix.m[13] * point.y + matrix.m[14] * point.z +
                matrix.m[15]};
}

Mat4 lookAt(const Camera& camera) {
    const Vec3 forward = normalize(camera.target - camera.position);
    const Vec3 side = normalize(cross(forward, camera.up));
    const Vec3 up = cross(side, forward);
    Mat4 matrix;
    matrix.m[0] = side.x;
    matrix.m[1] = side.y;
    matrix.m[2] = side.z;
    matrix.m[3] = -dot(side, camera.position);
    matrix.m[4] = up.x;
    matrix.m[5] = up.y;
    matrix.m[6] = up.z;
    matrix.m[7] = -dot(up, camera.position);
    matrix.m[8] = -forward.x;
    matrix.m[9] = -forward.y;
    matrix.m[10] = -forward.z;
    matrix.m[11] = dot(forward, camera.position);
    matrix.m[12] = 0.0f;
    matrix.m[13] = 0.0f;
    matrix.m[14] = 0.0f;
    matrix.m[15] = 1.0f;
    return matrix;
}

Mat4 perspective(const Camera& camera) {
    const float f = 1.0f / std::tan(camera.fovYDegrees * 3.14159265358979323846f / 360.0f);
    Mat4 matrix;
    matrix.m[0] = f / camera.aspect;
    matrix.m[5] = f;
    matrix.m[10] = (camera.farPlane + camera.nearPlane) /
                   (camera.nearPlane - camera.farPlane);
    matrix.m[11] = (2.0f * camera.farPlane * camera.nearPlane) /
                   (camera.nearPlane - camera.farPlane);
    matrix.m[14] = -1.0f;
    matrix.m[15] = 0.0f;
    return matrix;
}

struct ProjectedPoint {
    float x = 0.0f;
    float y = 0.0f;
    float depth = 1.0f;
    bool valid = false;
};

ProjectedPoint project(const Mat4& viewProjection, const Camera& camera, const Vec3& point) {
    const Vec4 clip = transform(viewProjection, point);
    if (clip.w <= kEpsilon) {
        return {};
    }
    const float ndcX = clip.x / clip.w;
    const float ndcY = clip.y / clip.w;
    const float ndcZ = clip.z / clip.w;
    ProjectedPoint projected;
    projected.x = (ndcX * 0.5f + 0.5f) * static_cast<float>(camera.width);
    projected.y = (1.0f - (ndcY * 0.5f + 0.5f)) * static_cast<float>(camera.height);
    projected.depth = ndcZ * 0.5f + 0.5f;
    projected.valid = std::isfinite(projected.depth) && projected.depth >= 0.0f &&
                      projected.depth <= 1.0f;
    return projected;
}

struct Plane {
    Vec3 normal;
    float distance = 0.0f;
};

std::array<Plane, 6> frustumPlanes(const Mat4& viewProjection) {
    auto row = [&](int index, int component) {
        return viewProjection.m[index * 4 + component];
    };
    auto makePlane = [](float a, float b, float c, float d) {
        const float magnitude = std::sqrt(a * a + b * b + c * c);
        if (magnitude <= kEpsilon) {
            return Plane{{0.0f, 0.0f, 0.0f}, -std::numeric_limits<float>::infinity()};
        }
        return Plane{{a / magnitude, b / magnitude, c / magnitude}, d / magnitude};
    };
    return {{
        makePlane(row(0, 3) + row(0, 0), row(1, 3) + row(1, 0),
                  row(2, 3) + row(2, 0), row(3, 3) + row(3, 0)),
        makePlane(row(0, 3) - row(0, 0), row(1, 3) - row(1, 0),
                  row(2, 3) - row(2, 0), row(3, 3) - row(3, 0)),
        makePlane(row(0, 3) + row(0, 1), row(1, 3) + row(1, 1),
                  row(2, 3) + row(2, 1), row(3, 3) + row(3, 1)),
        makePlane(row(0, 3) - row(0, 1), row(1, 3) - row(1, 1),
                  row(2, 3) - row(2, 1), row(3, 3) - row(3, 1)),
        makePlane(row(0, 3) + row(0, 2), row(1, 3) + row(1, 2),
                  row(2, 3) + row(2, 2), row(3, 3) + row(3, 2)),
        makePlane(row(0, 3) - row(0, 2), row(1, 3) - row(1, 2),
                  row(2, 3) - row(2, 2), row(3, 3) - row(3, 2))
    }};
}

bool aabbInsideFrustum(const Aabb& bounds, const std::array<Plane, 6>& planes) {
    const auto corners = bounds.corners();
    for (const Plane& plane : planes) {
        bool anyInside = false;
        for (const Vec3& corner : corners) {
            if (dot(plane.normal, corner) + plane.distance >= 0.0f) {
                anyInside = true;
                break;
            }
        }
        if (!anyInside) {
            return false;
        }
    }
    return true;
}

class DepthPyramid {
public:
    DepthPyramid(std::uint32_t width, std::uint32_t height)
        : width_(width), height_(height) {
        levels_.emplace_back(static_cast<std::size_t>(width) * height,
                             std::numeric_limits<float>::infinity());
    }

    void rasterizeOccluders(const std::vector<Chunk>& chunks,
                            const Mat4& viewProjection,
                            const Camera& camera) {
        for (const Chunk& chunk : chunks) {
            if (!chunk.occluder || !*chunk.occluder) {
                continue;
            }
            for (std::size_t index = 0; index + 2 < chunk.indices.size(); index += 3) {
                const Vec3& a = chunk.vertices[chunk.indices[index]];
                const Vec3& b = chunk.vertices[chunk.indices[index + 1]];
                const Vec3& c = chunk.vertices[chunk.indices[index + 2]];
                rasterizeTriangle(project(viewProjection, camera, a),
                                  project(viewProjection, camera, b),
                                  project(viewProjection, camera, c),
                                  camera);
            }
        }
        buildPyramid();
    }

    bool pointVisible(const ProjectedPoint& point,
                      float depthTolerance,
                      float projectedExtent = 1.0f) const {
        if (!point.valid) {
            return true;
        }
        const int x = clampCoordinate(static_cast<int>(std::floor(point.x)), width_);
        const int y = clampCoordinate(static_cast<int>(std::floor(point.y)), height_);
        const std::uint32_t level = selectMip(projectedExtent, projectedExtent);
        const auto& image = levels_[level];
        const std::uint32_t levelWidth = levelWidths_[level];
        const std::uint32_t levelHeight = levelHeights_[level];
        const int levelX = std::min<int>(static_cast<int>(x >> level),
                                         static_cast<int>(levelWidth) - 1);
        const int levelY = std::min<int>(static_cast<int>(y >> level),
                                         static_cast<int>(levelHeight) - 1);
        const float occluderDepth =
            image[static_cast<std::size_t>(levelY) * levelWidth +
                  static_cast<std::size_t>(levelX)];
        return !std::isfinite(occluderDepth) ||
               point.depth <= occluderDepth + depthTolerance;
    }

private:
    static int clampCoordinate(int value, std::uint32_t size) {
        if (value < 0) {
            return 0;
        }
        if (value >= static_cast<int>(size)) {
            return static_cast<int>(size) - 1;
        }
        return value;
    }

    std::uint32_t selectMip(float projectedWidth, float projectedHeight) const {
        const float extent = std::max(projectedWidth, projectedHeight);
        int mip = 0;
        if (extent > 8.0f) {
            mip = static_cast<int>(std::floor(std::log2(extent / 8.0f)));
        }
        mip = std::max(0, std::min<int>(mip, static_cast<int>(levels_.size()) - 1));
        return static_cast<std::uint32_t>(mip);
    }

    void rasterizeTriangle(const ProjectedPoint& a,
                           const ProjectedPoint& b,
                           const ProjectedPoint& c,
                           const Camera& camera) {
        if (!a.valid || !b.valid || !c.valid) {
            return;
        }
        const float minX = std::min({a.x, b.x, c.x});
        const float maxX = std::max({a.x, b.x, c.x});
        const float minY = std::min({a.y, b.y, c.y});
        const float maxY = std::max({a.y, b.y, c.y});
        const int startX = std::max(0, static_cast<int>(std::floor(minX)));
        const int endX =
            std::min(static_cast<int>(camera.width) - 1, static_cast<int>(std::ceil(maxX)));
        const int startY = std::max(0, static_cast<int>(std::floor(minY)));
        const int endY =
            std::min(static_cast<int>(camera.height) - 1, static_cast<int>(std::ceil(maxY)));
        if (startX > endX || startY > endY) {
            return;
        }
        const float area = (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x);
        if (std::abs(area) <= kEpsilon) {
            return;
        }
        auto& depth = levels_.front();
        for (int y = startY; y <= endY; ++y) {
            for (int x = startX; x <= endX; ++x) {
                const float sampleX = static_cast<float>(x) + 0.5f;
                const float sampleY = static_cast<float>(y) + 0.5f;
                float weightA =
                    ((b.x - sampleX) * (c.y - sampleY) -
                     (b.y - sampleY) * (c.x - sampleX)) /
                    area;
                float weightB =
                    ((c.x - sampleX) * (a.y - sampleY) -
                     (c.y - sampleY) * (a.x - sampleX)) /
                    area;
                float weightC = 1.0f - weightA - weightB;
                if (weightA < -kEpsilon || weightB < -kEpsilon ||
                    weightC < -kEpsilon) {
                    continue;
                }
                const float pointDepth =
                    weightA * a.depth + weightB * b.depth + weightC * c.depth;
                float& destination =
                    depth[static_cast<std::size_t>(y) * width_ +
                          static_cast<std::size_t>(x)];
                destination = std::min(destination, pointDepth);
            }
        }
    }

    void buildPyramid() {
        levelWidths_.clear();
        levelHeights_.clear();
        levelWidths_.push_back(width_);
        levelHeights_.push_back(height_);
        while (levelWidths_.back() > 1 || levelHeights_.back() > 1) {
            const std::uint32_t previousWidth = levelWidths_.back();
            const std::uint32_t previousHeight = levelHeights_.back();
            const std::uint32_t nextWidth = std::max(1u, (previousWidth + 1) / 2);
            const std::uint32_t nextHeight = std::max(1u, (previousHeight + 1) / 2);
            const auto& previous = levels_.back();
            std::vector<float> next(static_cast<std::size_t>(nextWidth) * nextHeight,
                                    std::numeric_limits<float>::infinity());
            for (std::uint32_t y = 0; y < nextHeight; ++y) {
                for (std::uint32_t x = 0; x < nextWidth; ++x) {
                    float value = std::numeric_limits<float>::infinity();
                    for (std::uint32_t offsetY = 0; offsetY < 2; ++offsetY) {
                        for (std::uint32_t offsetX = 0; offsetX < 2; ++offsetX) {
                            const std::uint32_t sourceX = x * 2 + offsetX;
                            const std::uint32_t sourceY = y * 2 + offsetY;
                            if (sourceX < previousWidth && sourceY < previousHeight) {
                                value = std::min(
                                    value,
                                    previous[static_cast<std::size_t>(sourceY) *
                                                previousWidth +
                                            sourceX]);
                            }
                        }
                    }
                    next[static_cast<std::size_t>(y) * nextWidth + x] = value;
                }
            }
            levels_.push_back(std::move(next));
            levelWidths_.push_back(nextWidth);
            levelHeights_.push_back(nextHeight);
        }
    }

    std::uint32_t width_;
    std::uint32_t height_;
    std::vector<std::vector<float>> levels_;
    std::vector<std::uint32_t> levelWidths_;
    std::vector<std::uint32_t> levelHeights_;
};

struct AdjacentPair {
    const Chunk* a = nullptr;
    const Chunk* b = nullptr;
    std::string segment;
    Vec3 axisA;
    Vec3 axisB;
    Vec3 boundaryOrigin;
    float boundaryCoordinate = 0.0f;
    char planeAxis = 'x';
};

std::vector<AdjacentPair> findAdjacentPairs(const std::map<std::uint32_t, Chunk>& loaded) {
    std::vector<AdjacentPair> pairs;
    std::vector<const Chunk*> chunks;
    chunks.reserve(loaded.size());
    for (const auto& entry : loaded) {
        chunks.push_back(&entry.second);
    }
    constexpr float contactTolerance = 1e-3f;
    for (std::size_t i = 0; i < chunks.size(); ++i) {
        for (std::size_t j = i + 1; j < chunks.size(); ++j) {
            const Chunk& a = *chunks[i];
            const Chunk& b = *chunks[j];
            auto overlaps = [](float aMin, float aMax, float bMin, float bMax) {
                return std::min(aMax, bMax) - std::max(aMin, bMin) > contactTolerance;
            };
            auto touching = [](float aMin, float aMax, float bMin, float bMax) {
                return std::abs(aMax - bMin) <= contactTolerance ||
                       std::abs(bMax - aMin) <= contactTolerance;
            };
            if (touching(a.bounds.min.x, a.bounds.max.x, b.bounds.min.x, b.bounds.max.x) &&
                overlaps(a.bounds.min.y, a.bounds.max.y, b.bounds.min.y, b.bounds.max.y) &&
                overlaps(a.bounds.min.z, a.bounds.max.z, b.bounds.min.z, b.bounds.max.z)) {
                const float boundary =
                    (std::abs(a.bounds.max.x - b.bounds.min.x) <
                     std::abs(b.bounds.max.x - a.bounds.min.x))
                        ? (a.bounds.max.x + b.bounds.min.x) * 0.5f
                        : (b.bounds.max.x + a.bounds.min.x) * 0.5f;
                AdjacentPair pair;
                pair.a = &a;
                pair.b = &b;
                pair.planeAxis = 'x';
                pair.boundaryCoordinate = boundary;
                pair.boundaryOrigin = {boundary,
                                       std::max(a.bounds.min.y, b.bounds.min.y),
                                       std::max(a.bounds.min.z, b.bounds.min.z)};
                pair.axisA = {0.0f, 1.0f, 0.0f};
                pair.axisB = {0.0f, 0.0f, 1.0f};
                std::ostringstream segment;
                segment << "x=" << std::fixed << std::setprecision(4) << boundary
                        << " y=[" << pair.boundaryOrigin.y << ","
                        << std::min(a.bounds.max.y, b.bounds.max.y) << "] z=["
                        << pair.boundaryOrigin.z << ","
                        << std::min(a.bounds.max.z, b.bounds.max.z) << "]";
                pair.segment = segment.str();
                pairs.push_back(pair);
            } else if (touching(a.bounds.min.z, a.bounds.max.z, b.bounds.min.z,
                                b.bounds.max.z) &&
                       overlaps(a.bounds.min.x, a.bounds.max.x, b.bounds.min.x,
                                b.bounds.max.x) &&
                       overlaps(a.bounds.min.y, a.bounds.max.y, b.bounds.min.y,
                                b.bounds.max.y)) {
                const float boundary =
                    (std::abs(a.bounds.max.z - b.bounds.min.z) <
                     std::abs(b.bounds.max.z - a.bounds.min.z))
                        ? (a.bounds.max.z + b.bounds.min.z) * 0.5f
                        : (b.bounds.max.z + a.bounds.min.z) * 0.5f;
                AdjacentPair pair;
                pair.a = &a;
                pair.b = &b;
                pair.planeAxis = 'z';
                pair.boundaryCoordinate = boundary;
                pair.boundaryOrigin = {std::max(a.bounds.min.x, b.bounds.min.x),
                                       std::max(a.bounds.min.y, b.bounds.min.y),
                                       boundary};
                pair.axisA = {1.0f, 0.0f, 0.0f};
                pair.axisB = {0.0f, 1.0f, 0.0f};
                std::ostringstream segment;
                segment << "z=" << std::fixed << std::setprecision(4) << boundary
                        << " x=[" << pair.boundaryOrigin.x << ","
                        << std::min(a.bounds.max.x, b.bounds.max.x) << "] y=["
                        << pair.boundaryOrigin.y << ","
                        << std::min(a.bounds.max.y, b.bounds.max.y) << "]";
                pair.segment = segment.str();
                pairs.push_back(pair);
            } else if (touching(a.bounds.min.y, a.bounds.max.y, b.bounds.min.y,
                                b.bounds.max.y) &&
                       overlaps(a.bounds.min.x, a.bounds.max.x, b.bounds.min.x,
                                b.bounds.max.x) &&
                       overlaps(a.bounds.min.z, a.bounds.max.z, b.bounds.min.z,
                                b.bounds.max.z)) {
                const float boundary =
                    (std::abs(a.bounds.max.y - b.bounds.min.y) <
                     std::abs(b.bounds.max.y - a.bounds.min.y))
                        ? (a.bounds.max.y + b.bounds.min.y) * 0.5f
                        : (b.bounds.max.y + a.bounds.min.y) * 0.5f;
                AdjacentPair pair;
                pair.a = &a;
                pair.b = &b;
                pair.planeAxis = 'y';
                pair.boundaryCoordinate = boundary;
                pair.boundaryOrigin = {std::max(a.bounds.min.x, b.bounds.min.x),
                                       boundary,
                                       std::max(a.bounds.min.z, b.bounds.min.z)};
                pair.axisA = {1.0f, 0.0f, 0.0f};
                pair.axisB = {0.0f, 0.0f, 1.0f};
                std::ostringstream segment;
                segment << "y=" << std::fixed << std::setprecision(4) << boundary
                        << " x=[" << pair.boundaryOrigin.x << ","
                        << std::min(a.bounds.max.x, b.bounds.max.x) << "] z=["
                        << pair.boundaryOrigin.z << ","
                        << std::min(a.bounds.max.z, b.bounds.max.z) << "]";
                pair.segment = segment.str();
                pairs.push_back(pair);
            }
        }
    }
    return pairs;
}

float boundaryVisibilityFraction(const AdjacentPair& pair,
                                 const Chunk& side,
                                 const Mat4& viewProjection,
                                 const Camera& camera,
                                 const DepthPyramid& pyramid,
                                 float depthTolerance) {
    const Vec3 normal = pair.planeAxis == 'x'
                            ? Vec3{1.0f, 0.0f, 0.0f}
                            : (pair.planeAxis == 'z' ? Vec3{0.0f, 0.0f, 1.0f}
                                                     : Vec3{0.0f, 1.0f, 0.0f});
    const float sideCoordinate = pair.planeAxis == 'x'
                                     ? side.bounds.center().x
                                     : (pair.planeAxis == 'z'
                                            ? side.bounds.center().z
                                            : side.bounds.center().y);
    const float direction = sideCoordinate < pair.boundaryCoordinate ? -1.0f : 1.0f;
    float rangeA = 0.0f;
    float rangeB = 0.0f;
    if (pair.planeAxis == 'x') {
        rangeA = std::min(pair.a->bounds.max.y, pair.b->bounds.max.y) -
                 std::max(pair.a->bounds.min.y, pair.b->bounds.min.y);
        rangeB = std::min(pair.a->bounds.max.z, pair.b->bounds.max.z) -
                 std::max(pair.a->bounds.min.z, pair.b->bounds.min.z);
    } else if (pair.planeAxis == 'z') {
        rangeA = std::min(pair.a->bounds.max.x, pair.b->bounds.max.x) -
                 std::max(pair.a->bounds.min.x, pair.b->bounds.min.x);
        rangeB = std::min(pair.a->bounds.max.y, pair.b->bounds.max.y) -
                 std::max(pair.a->bounds.min.y, pair.b->bounds.min.y);
    } else {
        rangeA = std::min(pair.a->bounds.max.x, pair.b->bounds.max.x) -
                 std::max(pair.a->bounds.min.x, pair.b->bounds.min.x);
        rangeB = std::min(pair.a->bounds.max.z, pair.b->bounds.max.z) -
                 std::max(pair.a->bounds.min.z, pair.b->bounds.min.z);
    }
    rangeA = std::max(0.0f, rangeA);
    rangeB = std::max(0.0f, rangeB);
    int visible = 0;
    int total = 0;
    for (int aIndex = -2; aIndex <= 2; ++aIndex) {
        for (int bIndex = -2; bIndex <= 2; ++bIndex) {
            const float fractionA = 0.5f + static_cast<float>(aIndex) * 0.225f;
            const float fractionB = 0.5f + static_cast<float>(bIndex) * 0.225f;
            Vec3 point = pair.boundaryOrigin + pair.axisA * (fractionA * rangeA) +
                         pair.axisB * (fractionB * rangeB);
            point = point + normal * (direction * 0.02f);
            const ProjectedPoint projected = project(viewProjection, camera, point);
            if (pyramid.pointVisible(projected, depthTolerance, 1.0f)) {
                ++visible;
            }
            ++total;
        }
    }
    return total == 0 ? 1.0f : static_cast<float>(visible) / static_cast<float>(total);
}

std::vector<std::uint32_t> sortedChunkIds(const SceneArchive& scene) {
    std::vector<std::uint32_t> ids;
    ids.reserve(scene.chunks.size());
    for (const Chunk& chunk : scene.chunks) {
        ids.push_back(chunk.id);
    }
    std::sort(ids.begin(), ids.end());
    return ids;
}

} // namespace

Result<FrameSnapshot> renderVisibility(const SceneArchive& scene,
                                       const std::vector<Chunk>& loadedChunks,
                                       const Camera& camera,
                                       float boundaryTolerance,
                                       const std::atomic<bool>* cancelRequested) {
    if (camera.width == 0 || camera.height == 0) {
        return Result<FrameSnapshot>::failure(
            Code::Internal, "offscreen framebuffer dimensions must be non-zero");
    }
    if (!std::isfinite(boundaryTolerance) || boundaryTolerance < 0.0f ||
        boundaryTolerance > 1.0f) {
        return Result<FrameSnapshot>::failure(
            Code::ChunkInvalid, "boundary tolerance must be finite and in [0, 1]");
    }

    std::map<std::uint32_t, Chunk> loaded;
    for (const Chunk& chunk : loadedChunks) {
        loaded[chunk.id] = chunk;
    }
    std::vector<Chunk> sortedLoaded;
    sortedLoaded.reserve(loaded.size());
    for (const auto& entry : loaded) {
        sortedLoaded.push_back(entry.second);
    }

    const Mat4 view = lookAt(camera);
    const Mat4 projection = perspective(camera);
    const Mat4 viewProjection = multiply(projection, view);
    const auto planes = frustumPlanes(viewProjection);
    const float depthTolerance = 1e-4f + boundaryTolerance * 1e-4f;
    DepthPyramid pyramid(camera.width, camera.height);
    pyramid.rasterizeOccluders(sortedLoaded, viewProjection, camera);

    FrameSnapshot snapshot;
    snapshot.archiveVersion = scene.version;
    snapshot.sceneId = scene.sceneId;
    snapshot.chunkIds = sortedChunkIds(scene);
    snapshot.visibilityMask.assign(snapshot.chunkIds.size(), 0);
    snapshot.stats.sceneChunks = static_cast<std::uint32_t>(scene.chunks.size());
    snapshot.stats.loadedChunks = static_cast<std::uint32_t>(loaded.size());

    std::map<std::uint32_t, std::size_t> chunkIndex;
    for (std::size_t index = 0; index < snapshot.chunkIds.size(); ++index) {
        chunkIndex[snapshot.chunkIds[index]] = index;
    }

    for (const Chunk& chunk : sortedLoaded) {
        if (cancelRequested &&
            cancelRequested->load(std::memory_order_acquire)) {
            BusinessError error;
            error.code = Code::Cancelled;
            error.message =
                "cull batch checkpoint observed a cooperative cancellation";
            ErrorLocation location;
            location.chunkId = chunk.id;
            location.field = "cull.checkpoint";
            error.locations.push_back(std::move(location));
            return Result<FrameSnapshot>::failure(std::move(error));
        }
        const std::size_t maskIndex = chunkIndex.at(chunk.id);
        VisibilityRecord record;
        record.chunkId = chunk.id;
        std::uint8_t flags = kMaskLoaded;
        if (chunk.occluder && *chunk.occluder) {
            flags |= kMaskOccluder;
        }
        if (!aabbInsideFrustum(chunk.bounds, planes)) {
            ++snapshot.stats.frustumCulled;
            snapshot.visibilityMask[maskIndex] = flags;
            record.flags = flags;
            snapshot.records.push_back(record);
            continue;
        }
        flags |= kMaskFrustumInside;

        std::vector<ProjectedPoint> samples;
        const auto corners = chunk.bounds.corners();
        float minScreenX = std::numeric_limits<float>::infinity();
        float maxScreenX = -std::numeric_limits<float>::infinity();
        float minScreenY = std::numeric_limits<float>::infinity();
        float maxScreenY = -std::numeric_limits<float>::infinity();
        for (const Vec3& corner : corners) {
            const ProjectedPoint projected = project(viewProjection, camera, corner);
            samples.push_back(projected);
            if (projected.valid) {
                minScreenX = std::min(minScreenX, projected.x);
                maxScreenX = std::max(maxScreenX, projected.x);
                minScreenY = std::min(minScreenY, projected.y);
                maxScreenY = std::max(maxScreenY, projected.y);
            }
        }
        samples.push_back(project(viewProjection, camera, chunk.bounds.center()));
        const float projectedExtent =
            std::isfinite(minScreenX)
                ? std::max(maxScreenX - minScreenX, maxScreenY - minScreenY)
                : 1.0f;
        std::size_t visibleSamples = 0;
        for (const ProjectedPoint& sample : samples) {
            if (pyramid.pointVisible(sample, depthTolerance, projectedExtent)) {
                ++visibleSamples;
            }
        }
        if (visibleSamples == 0) {
            ++snapshot.stats.occlusionCulled;
        } else {
            flags |= kMaskVisible;
            ++snapshot.stats.visibleChunks;
            if (visibleSamples < samples.size()) {
                flags |= kMaskCritical;
                ++snapshot.stats.criticalChunks;
            }
        }
        snapshot.visibilityMask[maskIndex] = flags;
        record.flags = flags;
        snapshot.records.push_back(record);
    }
    for (const Chunk& chunk : sortedLoaded) {
        snapshot.loadedChunkIds.push_back(chunk.id);
    }

    const auto adjacentPairs = findAdjacentPairs(loaded);
    snapshot.stats.boundaryPairs = static_cast<std::uint32_t>(adjacentPairs.size());
    for (const AdjacentPair& pair : adjacentPairs) {
        if (cancelRequested &&
            cancelRequested->load(std::memory_order_acquire)) {
            BusinessError error;
            error.code = Code::Cancelled;
            error.message =
                "boundary batch checkpoint observed a cooperative cancellation";
            ErrorLocation location;
            location.chunkId = pair.a->id;
            location.relatedChunkId = pair.b->id;
            location.field = "cull.boundary_checkpoint";
            location.segment = pair.segment;
            error.locations.push_back(std::move(location));
            return Result<FrameSnapshot>::failure(std::move(error));
        }
        const std::size_t indexA = chunkIndex.at(pair.a->id);
        const std::size_t indexB = chunkIndex.at(pair.b->id);
        const std::uint8_t maskA = snapshot.visibilityMask[indexA];
        const std::uint8_t maskB = snapshot.visibilityMask[indexB];
        if ((maskA & kMaskFrustumInside) == 0 || (maskB & kMaskFrustumInside) == 0) {
            continue;
        }
        BoundarySample sample;
        sample.chunkA = pair.a->id;
        sample.chunkB = pair.b->id;
        sample.segment = pair.segment;
        sample.fractionA =
            boundaryVisibilityFraction(pair, *pair.a, viewProjection, camera, pyramid,
                                       depthTolerance);
        sample.fractionB =
            boundaryVisibilityFraction(pair, *pair.b, viewProjection, camera, pyramid,
                                       depthTolerance);
        sample.consistent =
            std::abs(sample.fractionA - sample.fractionB) <= boundaryTolerance + 1e-5f;
        snapshot.boundarySamples.push_back(sample);
        auto recordFor = [&](std::uint32_t id) -> VisibilityRecord* {
            for (VisibilityRecord& record : snapshot.records) {
                if (record.chunkId == id) {
                    return &record;
                }
            }
            return nullptr;
        };
        if (auto* record = recordFor(sample.chunkA)) {
            record->boundaryVisibleFraction = sample.fractionA;
        }
        if (auto* record = recordFor(sample.chunkB)) {
            record->boundaryVisibleFraction = sample.fractionB;
        }
        if (!sample.consistent) {
            BusinessError error;
            error.code = Code::VisibilityMismatch;
            error.message =
                "adjacent chunks disagree at a shared visibility boundary";
            ErrorLocation location;
            location.chunkId = sample.chunkA;
            location.relatedChunkId = sample.chunkB;
            location.field = "visibility.boundary";
            location.segment = sample.segment;
            std::ostringstream detail;
            detail << "fraction_a=" << std::fixed << std::setprecision(4)
                   << sample.fractionA << " fraction_b=" << sample.fractionB
                   << " tolerance=" << boundaryTolerance;
            location.detail = detail.str();
            error.locations.push_back(std::move(location));
            return Result<FrameSnapshot>::failure(std::move(error));
        }
    }
    snapshot.contentHash = snapshotContentHash(snapshot);
    return Result<FrameSnapshot>::success(std::move(snapshot));
}

std::vector<Chunk> selectLoadedByPriority(const SceneArchive& scene,
                                          const Camera& camera,
                                          std::uint32_t maxChunks) {
    std::vector<Chunk> chunks = scene.chunks;
    std::sort(chunks.begin(), chunks.end(), [&](const Chunk& a, const Chunk& b) {
        if (a.priority != b.priority) {
            return a.priority > b.priority;
        }
        const float distanceA = length(a.bounds.center() - camera.position);
        const float distanceB = length(b.bounds.center() - camera.position);
        if (distanceA != distanceB) {
            return distanceA < distanceB;
        }
        return a.id < b.id;
    });
    if (maxChunks > 0 && chunks.size() > maxChunks) {
        chunks.resize(maxChunks);
    }
    std::sort(chunks.begin(), chunks.end(),
              [](const Chunk& a, const Chunk& b) { return a.id < b.id; });
    return chunks;
}

std::uint64_t snapshotContentHash(const FrameSnapshot& snapshot) {
    std::uint64_t hash = hashString("frame-snapshot-v1");
    hash = hashCombine(hash, snapshot.archiveVersion);
    hash = hashCombine(hash, snapshot.sceneId);
    hash = hashCombine(hash, snapshot.chunkIds.size());
    for (const std::uint32_t id : snapshot.chunkIds) {
        hash = hashCombine(hash, id);
    }
    hash = hashCombine(hash, snapshot.visibilityMask.size());
    for (const std::uint8_t value : snapshot.visibilityMask) {
        hash = hashCombine(hash, value);
    }
    hash = hashCombine(hash, snapshot.loadedChunkIds.size());
    for (const std::uint32_t id : snapshot.loadedChunkIds) {
        hash = hashCombine(hash, id);
    }
    for (const VisibilityRecord& record : snapshot.records) {
        hash = hashCombine(hash, record.chunkId);
        hash = hashCombine(hash, record.flags);
        std::uint32_t fractionBits = 0;
        std::memcpy(&fractionBits, &record.boundaryVisibleFraction,
                    sizeof(fractionBits));
        hash = hashCombine(hash, fractionBits);
    }
    for (const BoundarySample& sample : snapshot.boundarySamples) {
        hash = hashCombine(hash, sample.chunkA);
        hash = hashCombine(hash, sample.chunkB);
        hash = hashCombine(hash, hashString(sample.segment));
        std::uint32_t fractionBits = 0;
        std::memcpy(&fractionBits, &sample.fractionA, sizeof(fractionBits));
        hash = hashCombine(hash, fractionBits);
        std::memcpy(&fractionBits, &sample.fractionB, sizeof(fractionBits));
        hash = hashCombine(hash, fractionBits);
        hash = hashCombine(hash, sample.consistent ? 1 : 0);
    }
    hash = hashCombine(hash, snapshot.stats.sceneChunks);
    hash = hashCombine(hash, snapshot.stats.loadedChunks);
    hash = hashCombine(hash, snapshot.stats.frustumCulled);
    hash = hashCombine(hash, snapshot.stats.occlusionCulled);
    hash = hashCombine(hash, snapshot.stats.visibleChunks);
    hash = hashCombine(hash, snapshot.stats.criticalChunks);
    hash = hashCombine(hash, snapshot.stats.boundaryPairs);
    return hash;
}

Result<void> checkBoundaryConsistency(
    const std::vector<BoundarySample>& samples,
    float boundaryTolerance) {
    for (const BoundarySample& sample : samples) {
        const bool consistent =
            sample.consistent &&
            std::abs(sample.fractionA - sample.fractionB) <=
                boundaryTolerance + 1e-5f;
        if (consistent) {
            continue;
        }
        BusinessError error;
        error.code = Code::VisibilityMismatch;
        error.message = "adjacent chunks disagree at a shared visibility boundary";
        ErrorLocation location;
        location.chunkId = sample.chunkA;
        location.relatedChunkId = sample.chunkB;
        location.field = "visibility.boundary";
        location.segment = sample.segment;
        std::ostringstream detail;
        detail << "fraction_a=" << std::fixed << std::setprecision(4)
               << sample.fractionA << " fraction_b=" << sample.fractionB
               << " tolerance=" << boundaryTolerance;
        location.detail = detail.str();
        error.locations.push_back(std::move(location));
        return Result<void>::failure(std::move(error));
    }
    return Result<void>::success();
}

std::string maskHex(const FrameSnapshot& snapshot) {
    std::ostringstream stream;
    stream << std::hex << std::setfill('0');
    for (const std::uint8_t value : snapshot.visibilityMask) {
        stream << std::setw(2) << static_cast<unsigned>(value);
    }
    return stream.str();
}

} // namespace chunkpipe

