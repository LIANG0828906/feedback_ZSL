#include "render.h"

#include "hash.h"
#include "output.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstring>
#include <limits>
#include <sstream>
#include <tuple>

namespace cr {
namespace {

struct Hit {
    const Mesh* mesh = nullptr;
    const Layer* layer = nullptr;
    ComponentType type = ComponentType::Board;
    int ownerCode = 0;
    std::int64_t triangleIndex = 0;
    double t = 0.0;
    Vec3 point{};
    Vec2 uv{};
    Vec3 normal{};
    double componentThickness = 0.0;
    double materialAmount = 0.0;
    Vec3 color{};
};

struct ShadedSurface {
    Vec3 premultiplied{};
    double opacity = 0.0;
    double transmit = 1.0;
    int owner = kBackgroundOwnerCode;
    double reflectedEnergy = 0.0;
    double transmittedEnergy = 0.0;
};

bool intersectTriangle(const Ray& ray, const Mesh& mesh, const Triangle& tri, double nearZ,
                       double farZ, double& t, Barycentric& bary) {
    const Vec3& a = mesh.vertices[static_cast<std::size_t>(tri.indices[0])];
    const Vec3& b = mesh.vertices[static_cast<std::size_t>(tri.indices[1])];
    const Vec3& c = mesh.vertices[static_cast<std::size_t>(tri.indices[2])];
    const Vec3 e1 = b - a;
    const Vec3 e2 = c - a;
    const Vec3 pvec = cross(ray.direction, e2);
    const double det = dot(e1, pvec);
    if (std::abs(det) < 1e-13) return false;
    const double invDet = 1.0 / det;
    const Vec3 tvec = ray.origin - a;
    const double u = dot(tvec, pvec) * invDet;
    if (u < -1e-10 || u > 1.0 + 1e-10) return false;
    const Vec3 qvec = cross(tvec, e1);
    const double v = dot(ray.direction, qvec) * invDet;
    if (v < -1e-10 || u + v > 1.0 + 1e-10) return false;
    const double distance = dot(e2, qvec) * invDet;
    if (!(distance > nearZ) || !(distance < farZ)) return false;
    t = distance;
    bary = {1.0 - u - v, u, v};
    return true;
}

Vec3 triangleNormal(const Mesh& mesh, const Triangle& tri) {
    const Vec3& a = mesh.vertices[static_cast<std::size_t>(tri.indices[0])];
    const Vec3& b = mesh.vertices[static_cast<std::size_t>(tri.indices[1])];
    const Vec3& c = mesh.vertices[static_cast<std::size_t>(tri.indices[2])];
    return normalize(cross(b - a, c - a));
}

double lightAttenuation(double distance) {
    return 1.0 / (1.0 + 0.30 * distance * distance);
}

Vec3 toneMap(Vec3 linear, double exposure) {
    const Vec3 scaled = linear * exposure;
    return {1.0 - std::exp(-std::max(0.0, scaled.x)),
            1.0 - std::exp(-std::max(0.0, scaled.y)),
            1.0 - std::exp(-std::max(0.0, scaled.z))};
}

std::uint8_t toByte(double value) {
    return static_cast<std::uint8_t>(
        std::lround(clamp01(value) * 255.0));
}

}  // namespace

struct RenderContext::ShadowMap {
    int lightIndex = 0;
    int occluderLayer = -1;
    int targetKey = -1;
    int targetLayer = -1;
    double targetZ = 0.0;
    int resolution = 0;
    double minX = -1.0;
    double maxX = 1.0;
    double minY = -1.0;
    double maxY = 1.0;
    double clipMinX = -std::numeric_limits<double>::infinity();
    double clipMaxX = std::numeric_limits<double>::infinity();
    double clipMinY = -std::numeric_limits<double>::infinity();
    double clipMaxY = std::numeric_limits<double>::infinity();
    std::vector<float> mask;

    double softSample(Vec2 point, double radius, int samplesPerAxis,
                      std::uint64_t seed) const;

    bool contains(double x, double y) const {
        return x >= minX && x <= maxX && y >= minY && y <= maxY;
    }

    bool containsClip(double x, double y) const {
        return x >= clipMinX && x <= clipMaxX && y >= clipMinY && y <= clipMaxY;
    }

    double sample(double x, double y) const {
        if (!contains(x, y)) return 1.0;
        const double px =
            (x - minX) / (maxX - minX) * static_cast<double>(resolution) - 0.5;
        const double py =
            (y - minY) / (maxY - minY) * static_cast<double>(resolution) - 0.5;
        const int x0 = static_cast<int>(std::floor(px));
        const int y0 = static_cast<int>(std::floor(py));
        const double tx = px - static_cast<double>(x0);
        const double ty = py - static_cast<double>(y0);
        auto at = [&](int sx, int sy) -> double {
            if (sx < 0 || sy < 0 || sx >= resolution || sy >= resolution) return 1.0;
            return mask[static_cast<std::size_t>(sy * resolution + sx)];
        };
        const double a = mix(at(x0, y0), at(x0 + 1, y0), tx);
        const double b = mix(at(x0, y0 + 1), at(x0 + 1, y0 + 1), tx);
        return mix(a, b, ty);
    }

    void rasterizeProjectedTriangle(const Vec3& a, const Vec3& b, const Vec3& c,
                                    const Vec3& light, double targetZ) {
        auto project = [&](const Vec3& vertex, Vec2& out) -> bool {
            const double dz = vertex.z - light.z;
            if (std::abs(dz) < 1e-10) return false;
            const double t = (targetZ - light.z) / dz;
            if (!std::isfinite(t)) return false;
            out = {light.x + t * (vertex.x - light.x),
                   light.y + t * (vertex.y - light.y)};
            return std::isfinite(out.x) && std::isfinite(out.y);
        };
        Vec2 pa;
        Vec2 pb;
        Vec2 pc;
        if (!project(a, pa) || !project(b, pb) || !project(c, pc)) return;
        const double area = (pb.x - pa.x) * (pc.y - pa.y) -
                            (pc.x - pa.x) * (pb.y - pa.y);
        if (std::abs(area) < 1e-16) return;
        double triMinX = std::min({pa.x, pb.x, pc.x});
        double triMaxX = std::max({pa.x, pb.x, pc.x});
        double triMinY = std::min({pa.y, pb.y, pc.y});
        double triMaxY = std::max({pa.y, pb.y, pc.y});
        triMinX = std::max(triMinX, clipMinX);
        triMaxX = std::min(triMaxX, clipMaxX);
        triMinY = std::max(triMinY, clipMinY);
        triMaxY = std::min(triMaxY, clipMaxY);
        if (triMaxX < triMinX || triMaxY < triMinY) return;
        int sx0 = static_cast<int>(std::floor(
            (triMinX - minX) / (maxX - minX) * static_cast<double>(resolution) - 0.5));
        int sx1 = static_cast<int>(std::ceil(
            (triMaxX - minX) / (maxX - minX) * static_cast<double>(resolution) - 0.5));
        int sy0 = static_cast<int>(std::floor(
            (triMinY - minY) / (maxY - minY) * static_cast<double>(resolution) - 0.5));
        int sy1 = static_cast<int>(std::ceil(
            (triMaxY - minY) / (maxY - minY) * static_cast<double>(resolution) - 0.5));
        sx0 = std::max(0, sx0);
        sy0 = std::max(0, sy0);
        sx1 = std::min(resolution - 1, sx1);
        sy1 = std::min(resolution - 1, sy1);
        constexpr int kSub = 2;
        for (int sy = sy0; sy <= sy1; ++sy) {
            for (int sx = sx0; sx <= sx1; ++sx) {
                int covered = 0;
                for (int oy = 0; oy < kSub; ++oy) {
                    for (int ox = 0; ox < kSub; ++ox) {
                        const double fx = static_cast<double>(sx) +
                                          (static_cast<double>(ox) + 0.5) /
                                              static_cast<double>(kSub);
                        const double fy = static_cast<double>(sy) +
                                          (static_cast<double>(oy) + 0.5) /
                                              static_cast<double>(kSub);
                        const double x =
                            minX + (fx + 0.5) / static_cast<double>(resolution) *
                                       (maxX - minX);
                        const double y =
                            minY + (fy + 0.5) / static_cast<double>(resolution) *
                                       (maxY - minY);
                        if (!containsClip(x, y)) continue;
                        Barycentric bary;
                        if (barycentric2D({x, y}, pa, pb, pc, bary)) ++covered;
                    }
                }
                if (covered == 0) continue;
                const double coverage =
                    static_cast<double>(covered) / static_cast<double>(kSub * kSub);
                float& pixel = mask[static_cast<std::size_t>(sy * resolution + sx)];
                pixel = static_cast<float>(std::max<double>(pixel, coverage));
            }
        }
    }
};

struct RenderContext::Impl {
    std::map<std::tuple<int, int, int>, ShadowMap> maps;
    double shadowMinX = -1.0;
    double shadowMaxX = 1.0;
    double shadowMinY = -1.0;
    double shadowMaxY = 1.0;

    const ShadowMap* map(int light, int occluderLayer, int targetKey) const {
        const auto it = maps.find({light, occluderLayer, targetKey});
        return it == maps.end() ? nullptr : &it->second;
    }

    const Layer* layerByIndex(int index) const {
        if (scene == nullptr) return nullptr;
        const Scene& s = *scene;
        const auto it = std::lower_bound(
            s.layers.begin(), s.layers.end(), index,
            [](const Layer& layer, int value) { return layer.index < value; });
        return it != s.layers.end() && it->index == index ? &*it : nullptr;
    }

    const Scene* scene = nullptr;
};

namespace {

Vec3 meshCentroid(const Mesh& mesh) {
    Vec3 sum{};
    if (mesh.vertices.empty()) return sum;
    for (const Vec3& vertex : mesh.vertices) sum += vertex;
    return sum / static_cast<double>(mesh.vertices.size());
}

double computePenumbraRadius(const Scene& scene, const Light& light, double occluderZ,
                             double targetZ) {
    const double receiverDistance = std::abs(targetZ - occluderZ);
    const double lightDistance = std::abs(occluderZ - light.position.z);
    return scene.render.penumbraScale * (1.0 + 3.0 * receiverDistance) *
           (1.0 + 1.5 * lightDistance);
}

std::vector<Vec2> diskOffsets(int samplesPerAxis, std::uint64_t rotationSeed) {
    const int count = samplesPerAxis * samplesPerAxis;
    std::vector<Vec2> offsets;
    offsets.reserve(static_cast<std::size_t>(count));
    const double rotation =
        static_cast<double>(rotationSeed % 1048576u) / 1048576.0 * 2.0 * kPi;
    for (int i = 0; i < count; ++i) {
        const double r = std::sqrt((static_cast<double>(i) + 0.5) /
                                   static_cast<double>(count));
        const double theta = rotation + static_cast<double>(i) *
                                           2.39996322972865332223;
        offsets.push_back({r * std::cos(theta), r * std::sin(theta)});
    }
    return offsets;
}

}  // namespace

double RenderContext::ShadowMap::softSample(Vec2 point, double radius, int samplesPerAxis,
                                            std::uint64_t seed) const {
    if (!containsClip(point.x, point.y)) return 1.0;
    const std::vector<Vec2> offsets = diskOffsets(samplesPerAxis, seed);
    double sum = 0.0;
    for (const Vec2& offset : offsets) {
        sum += 1.0 - sample(point.x + offset.x * radius, point.y + offset.y * radius);
    }
    return clamp01(sum / static_cast<double>(offsets.size()));
}

RenderContext::RenderContext(Scene sceneIn)
    : scene(std::move(sceneIn)), textures(buildTextureBank(scene)),
      ownerTable(buildOwnerTable(scene)), impl_(std::make_shared<Impl>()) {
    Impl& impl = *impl_;
    impl.scene = &scene;

    impl.shadowMinX = std::numeric_limits<double>::infinity();
    impl.shadowMinY = std::numeric_limits<double>::infinity();
    impl.shadowMaxX = -std::numeric_limits<double>::infinity();
    impl.shadowMaxY = -std::numeric_limits<double>::infinity();
    auto includePoint = [&](Vec3 point) {
        impl.shadowMinX = std::min(impl.shadowMinX, point.x);
        impl.shadowMaxX = std::max(impl.shadowMaxX, point.x);
        impl.shadowMinY = std::min(impl.shadowMinY, point.y);
        impl.shadowMaxY = std::max(impl.shadowMaxY, point.y);
    };
    for (const Layer& layer : scene.layers) {
        for (const Board& board : layer.boards) {
            for (const Vec3& vertex : board.mesh.vertices) includePoint(vertex);
        }
        for (const Bracket& bracket : layer.brackets) {
            for (const Vec3& vertex : bracket.mesh.vertices) includePoint(vertex);
        }
    }
    includePoint({scene.receiver.minX, scene.receiver.minY, scene.receiver.z});
    includePoint({scene.receiver.maxX, scene.receiver.maxY, scene.receiver.z});
    const double spanX = std::max(1e-6, impl.shadowMaxX - impl.shadowMinX);
    const double spanY = std::max(1e-6, impl.shadowMaxY - impl.shadowMinY);
    const double margin = std::max(0.02, std::max(spanX, spanY) * 0.06);
    impl.shadowMinX -= margin;
    impl.shadowMaxX += margin;
    impl.shadowMinY -= margin;
    impl.shadowMaxY += margin;

    auto boundsForLayer = [&](const Layer& layer, double& minX, double& maxX, double& minY,
                              double& maxY) {
        minX = std::numeric_limits<double>::infinity();
        maxX = -std::numeric_limits<double>::infinity();
        minY = std::numeric_limits<double>::infinity();
        maxY = -std::numeric_limits<double>::infinity();
        for (const Board& board : layer.boards) {
            for (const Vec3& vertex : board.mesh.vertices) {
                minX = std::min(minX, vertex.x);
                maxX = std::max(maxX, vertex.x);
                minY = std::min(minY, vertex.y);
                maxY = std::max(maxY, vertex.y);
            }
        }
        if (!std::isfinite(minX)) {
            minX = impl.shadowMinX;
            maxX = impl.shadowMaxX;
            minY = impl.shadowMinY;
            maxY = impl.shadowMaxY;
        }
    };

    auto addMap = [&](int lightIndex, const Layer& occluderLayer, int targetKey,
                      int targetLayer, double targetZ, bool receiverTarget) {
        ShadowMap map;
        map.lightIndex = lightIndex;
        map.occluderLayer = occluderLayer.index;
        map.targetKey = targetKey;
        map.targetLayer = targetLayer;
        map.targetZ = targetZ;
        map.resolution = scene.render.shadowMapResolution;
        map.minX = impl.shadowMinX;
        map.maxX = impl.shadowMaxX;
        map.minY = impl.shadowMinY;
        map.maxY = impl.shadowMaxY;
        map.mask.assign(static_cast<std::size_t>(map.resolution) *
                            static_cast<std::size_t>(map.resolution),
                        0.0f);
        if (receiverTarget) {
            map.clipMinX = scene.receiver.minX;
            map.clipMaxX = scene.receiver.maxX;
            map.clipMinY = scene.receiver.minY;
            map.clipMaxY = scene.receiver.maxY;
        } else {
            const Layer* target = impl.layerByIndex(targetLayer);
            if (target != nullptr) {
                double minX = 0.0;
                double maxX = 0.0;
                double minY = 0.0;
                double maxY = 0.0;
                boundsForLayer(*target, minX, maxX, minY, maxY);
                const double layerMargin =
                    std::max(0.01, std::max(maxX - minX, maxY - minY) * 0.02);
                map.clipMinX = minX - layerMargin;
                map.clipMaxX = maxX + layerMargin;
                map.clipMinY = minY - layerMargin;
                map.clipMaxY = maxY + layerMargin;
            }
        }
        const Light& light = scene.lights[static_cast<std::size_t>(lightIndex)];
        for (const Bracket& bracket : occluderLayer.brackets) {
            for (const Triangle& tri : bracket.mesh.triangles) {
                const Vec3& a = bracket.mesh.vertices[static_cast<std::size_t>(tri.indices[0])];
                const Vec3& b = bracket.mesh.vertices[static_cast<std::size_t>(tri.indices[1])];
                const Vec3& c = bracket.mesh.vertices[static_cast<std::size_t>(tri.indices[2])];
                map.rasterizeProjectedTriangle(a, b, c, light.position, targetZ);
            }
        }
        impl.maps[{lightIndex, occluderLayer.index, targetKey}] = std::move(map);
    };

    for (std::size_t li = 0; li < scene.lights.size(); ++li) {
        const int lightIndex = static_cast<int>(li);
        for (const Layer& occluderLayer : scene.layers) {
            if (occluderLayer.brackets.empty()) continue;
            if (occluderLayer.z < scene.receiver.z) {
                addMap(lightIndex, occluderLayer, kReceiverTargetKey, -1, scene.receiver.z, true);
            }
            for (const Layer& targetLayer : scene.layers) {
                if (targetLayer.z <= occluderLayer.z + 1e-9) continue;
                addMap(lightIndex, occluderLayer, targetLayer.index, targetLayer.index,
                       targetLayer.z, false);
            }
        }
    }

    auto targetBounds = [&](int targetLayer, double& minX, double& maxX, double& minY,
                            double& maxY) {
        if (targetLayer < 0) {
            minX = scene.receiver.minX;
            maxX = scene.receiver.maxX;
            minY = scene.receiver.minY;
            maxY = scene.receiver.maxY;
            return;
        }
        const Layer* layer = impl.layerByIndex(targetLayer);
        if (layer == nullptr) {
            minX = impl.shadowMinX;
            maxX = impl.shadowMaxX;
            minY = impl.shadowMinY;
            maxY = impl.shadowMaxY;
            return;
        }
        boundsForLayer(*layer, minX, maxX, minY, maxY);
    };

    auto projectPoint = [](Vec3 point, const Light& light, double targetZ, Vec3& out) {
        const double dz = point.z - light.position.z;
        if (std::abs(dz) < 1e-10) return false;
        const double t = (targetZ - light.position.z) / dz;
        if (!std::isfinite(t)) return false;
        out = {light.position.x + t * (point.x - light.position.x),
               light.position.y + t * (point.y - light.position.y), targetZ};
        return true;
    };

    for (std::size_t lightIndex = 0; lightIndex < scene.lights.size(); ++lightIndex) {
        const Light& light = scene.lights[lightIndex];
        for (const Layer& occluderLayer : scene.layers) {
            if (occluderLayer.brackets.empty()) continue;
            std::vector<std::pair<int, double>> targets;
            if (occluderLayer.z < scene.receiver.z) targets.emplace_back(-1, scene.receiver.z);
            for (const Layer& targetLayer : scene.layers) {
                if (targetLayer.z > occluderLayer.z + 1e-9) {
                    targets.emplace_back(targetLayer.index, targetLayer.z);
                }
            }
            for (const auto& [targetLayer, targetZ] : targets) {
                double minX = 0.0;
                double maxX = 0.0;
                double minY = 0.0;
                double maxY = 0.0;
                targetBounds(targetLayer, minX, maxX, minY, maxY);
                for (const Bracket& bracket : occluderLayer.brackets) {
                    Vec3 point;
                    if (!projectPoint(meshCentroid(bracket.mesh), light, targetZ, point)) continue;
                    ProjectionLanding landing;
                    landing.lightIndex = static_cast<int>(lightIndex);
                    landing.lightId = light.id;
                    landing.target = targetLayer < 0
                                         ? "receiver"
                                         : "board_layer_" + std::to_string(targetLayer);
                    landing.targetLayer = targetLayer;
                    landing.occluderLayer = occluderLayer.index;
                    landing.componentId = bracket.id;
                    landing.origin = meshCentroid(bracket.mesh);
                    landing.point = point;
                    landing.uv = {(point.x - minX) / std::max(1e-12, maxX - minX),
                                  (point.y - minY) / std::max(1e-12, maxY - minY)};
                    landing.inBounds =
                        point.x >= minX && point.x <= maxX && point.y >= minY &&
                        point.y <= maxY;
                    landing.treatedAsUnoccluded = !landing.inBounds;
                    landings.push_back(std::move(landing));
                }
            }
        }
    }
    std::sort(landings.begin(), landings.end(),
              [](const ProjectionLanding& a, const ProjectionLanding& b) {
                  return std::tie(a.lightIndex, a.targetLayer, a.occluderLayer,
                                  a.componentId) <
                         std::tie(b.lightIndex, b.targetLayer, b.occluderLayer,
                                  b.componentId);
              });

    for (const auto& entry : impl.maps) {
        const ShadowMap& map = entry.second;
        const Light& light = scene.lights[static_cast<std::size_t>(map.lightIndex)];
        const Layer* occluderLayer = impl.layerByIndex(map.occluderLayer);
        if (occluderLayer == nullptr || occluderLayer->brackets.empty()) continue;
        double minX = 0.0;
        double maxX = 0.0;
        double minY = 0.0;
        double maxY = 0.0;
        targetBounds(map.targetLayer, minX, maxX, minY, maxY);
        bool found = false;
        Vec2 projectedCenter;
        for (const Bracket& bracket : occluderLayer->brackets) {
            Vec3 point;
            if (!projectPoint(meshCentroid(bracket.mesh), light, map.targetZ, point)) continue;
            if (point.x < minX || point.x > maxX || point.y < minY || point.y > maxY) continue;
            projectedCenter = {point.x, point.y};
            found = true;
            break;
        }
        if (!found) continue;
        const double radius =
            computePenumbraRadius(scene, light, occluderLayer->z, map.targetZ);
        const int samples = scene.render.projectionSamples;
        const std::uint64_t seed =
            splitmix64(static_cast<std::uint64_t>(map.lightIndex + 1) *
                           0x9e3779b97f4a7c15ull ^
                       static_cast<std::uint64_t>(map.occluderLayer + 17) *
                           0xbf58476d1ce4e5b9ull ^
                       static_cast<std::uint64_t>(map.targetKey + 101) *
                           0x94d049bb133111ebull);
        auto brightnessAt = [&](double xOffset) {
            return map.softSample({projectedCenter.x + xOffset, projectedCenter.y}, radius,
                                  samples, seed);
        };
        const double halfWidth = std::max(0.03, radius * 5.0);
        constexpr int kScanCount = 65;
        std::vector<double> offsets;
        std::vector<double> brightness;
        offsets.reserve(kScanCount);
        brightness.reserve(kScanCount);
        for (int i = 0; i < kScanCount; ++i) {
            const double t = static_cast<double>(i) / static_cast<double>(kScanCount - 1);
            const double offset = mix(-halfWidth, halfWidth, t);
            offsets.push_back(offset);
            brightness.push_back(brightnessAt(offset));
        }
        auto crossing = [&](bool positive) {
            double previousOffset = offsets[32];
            double previousValue = brightness[32];
            for (int i = 33; i < kScanCount; ++i) {
                const double currentOffset = positive ? offsets[i] : -offsets[i];
                const double currentValue = brightness[i];
                if (previousValue >= 0.5 && currentValue < 0.5) {
                    const double denominator = previousValue - currentValue;
                    const double alpha = denominator > 1e-12
                                             ? (previousValue - 0.5) / denominator
                                             : 0.0;
                    return mix(previousOffset, currentOffset, alpha);
                }
                if (currentValue >= 0.5) {
                    previousOffset = currentOffset;
                    previousValue = currentValue;
                }
            }
            return positive ? halfWidth : -halfWidth;
        };
        const double left = crossing(false);
        const double right = crossing(true);
        PenumbraComparison comparison;
        comparison.lightIndex = map.lightIndex;
        comparison.lightId = light.id;
        comparison.target = map.targetLayer < 0
                                ? "receiver"
                                : "board_layer_" + std::to_string(map.targetLayer);
        comparison.targetLayer = map.targetLayer;
        comparison.occluderLayer = map.occluderLayer;
        comparison.receiverDistance = std::abs(map.targetZ - occluderLayer->z);
        comparison.lightOccluderDistance = std::abs(occluderLayer->z - light.position.z);
        comparison.penumbraRadius = radius;
        comparison.penumbraWidth = 2.0 * radius;
        comparison.edgeWidth50 = std::max(0.0, right - left);
        for (int i = 0; i < 9; ++i) {
            const double t = static_cast<double>(i) / 8.0;
            const double offset = mix(-2.0 * radius, 2.0 * radius, t);
            comparison.sampleOffsets.push_back(offset);
            comparison.sampleBrightness.push_back(brightnessAt(offset));
        }
        penumbraComparisons.push_back(std::move(comparison));
    }

    projectionDigest = sha256Hex(projectionCanonical());
}

json::Value LayerEnergyStats::toJson(int layerIndex) const {
    json::Value result = json::Value::object();
    result.set("layer", json::Value(layerIndex));
    result.set("samples", json::Value(static_cast<double>(samples)));
    result.set("reflectedContribution", json::Value(reflected));
    result.set("transmittedContribution", json::Value(transmitted));
    const double total = reflected + transmitted;
    const double reflectedShare = total > 0.0 ? reflected / total : 0.0;
    const double transmittedShare = total > 0.0 ? transmitted / total : 0.0;
    result.set("reflectedShare", json::Value(reflectedShare));
    result.set("transmittedShare", json::Value(transmittedShare));
    return result;
}

json::Value BlockStats::toJson() const {
    json::Value result = json::Value::object();
    result.set("rays", json::Value(static_cast<double>(rays)));
    json::Value layers = json::Value::array();
    for (std::size_t i = 0; i < this->layers.size(); ++i) {
        layers.push(this->layers[i].toJson(this->layers[i].layerIndex));
    }
    result.set("layers", std::move(layers));
    return result;
}

std::string TileData::rgbaDigest() const {
    return sha256Hex(rgba.data(), rgba.size());
}

std::string TileData::ownerDigest() const {
    const std::vector<std::uint8_t> packed = ownerBinary(owners, width, height);
    return sha256Hex(packed.data(), packed.size());
}

Vec3 TileData::averageColor() const {
    Vec3 sum{};
    if (rgba.empty()) return sum;
    for (std::size_t i = 0; i < rgba.size(); i += 4) {
        sum.x += static_cast<double>(rgba[i]) / 255.0;
        sum.y += static_cast<double>(rgba[i + 1]) / 255.0;
        sum.z += static_cast<double>(rgba[i + 2]) / 255.0;
    }
    return sum / static_cast<double>(rgba.size() / 4);
}

json::Value TileData::summaryJson() const {
    json::Value result = json::Value::object();
    result.set("index", json::Value(index));
    result.set("x", json::Value(x));
    result.set("y", json::Value(y));
    result.set("width", json::Value(width));
    result.set("height", json::Value(height));
    result.set("rgbaDigest", json::Value(rgbaDigest()));
    result.set("ownerDigest", json::Value(ownerDigest()));
    const Vec3 average = averageColor();
    json::Value avg = json::Value::array();
    avg.push(json::Value(average.x));
    avg.push(json::Value(average.y));
    avg.push(json::Value(average.z));
    avg.push(json::Value(1.0));
    result.set("averageRGBA", std::move(avg));
    result.set("stats", stats.toJson());
    return result;
}

json::Value RenderContext::projectionReport() const {
    json::Value root = json::Value::object();
    root.set("format", json::Value("coffered-projection/v1"));
    root.set("projectionSamples", json::Value(scene.render.projectionSamples));
    root.set("shadowMapResolution", json::Value(scene.render.shadowMapResolution));
    root.set("penumbraScale", json::Value(scene.render.penumbraScale));

    json::Value lightsJson = json::Value::array();
    for (std::size_t li = 0; li < scene.lights.size(); ++li) {
        const Light& light = scene.lights[li];
        json::Value lightJson = json::Value::object();
        lightJson.set("id", json::Value(light.id));
        lightJson.set("position", [&]() {
            json::Value position = json::Value::array();
            position.push(json::Value(light.position.x));
            position.push(json::Value(light.position.y));
            position.push(json::Value(light.position.z));
            return position;
        }());
        json::Value list = json::Value::array();
        for (const ProjectionLanding& landing : landings) {
            if (landing.lightIndex != static_cast<int>(li)) continue;
            json::Value item = json::Value::object();
            item.set("target", json::Value(landing.target));
            if (landing.targetLayer >= 0) {
                item.set("targetLayer", json::Value(landing.targetLayer));
            }
            item.set("occluderLayer", json::Value(landing.occluderLayer));
            item.set("componentId", json::Value(landing.componentId));
            auto pointJson = [](Vec3 point) {
                json::Value result = json::Value::array();
                result.push(json::Value(point.x));
                result.push(json::Value(point.y));
                result.push(json::Value(point.z));
                return result;
            };
            item.set("origin", pointJson(landing.origin));
            item.set("point", pointJson(landing.point));
            json::Value uv = json::Value::array();
            uv.push(json::Value(landing.uv.x));
            uv.push(json::Value(landing.uv.y));
            item.set("uv", std::move(uv));
            item.set("inBounds", json::Value(landing.inBounds));
            item.set("treatedAsUnoccluded",
                     json::Value(landing.treatedAsUnoccluded));
            list.push(std::move(item));
        }
        lightJson.set("landings", std::move(list));
        lightsJson.push(std::move(lightJson));
    }
    root.set("lights", std::move(lightsJson));

    json::Value comparisons = json::Value::array();
    for (const PenumbraComparison& comparison : penumbraComparisons) {
        json::Value item = json::Value::object();
        item.set("light", json::Value(comparison.lightId));
        item.set("target", json::Value(comparison.target));
        if (comparison.targetLayer >= 0) {
            item.set("targetLayer", json::Value(comparison.targetLayer));
        }
        item.set("occluderLayer", json::Value(comparison.occluderLayer));
        item.set("receiverDistance", json::Value(comparison.receiverDistance));
        item.set("lightOccluderDistance",
                 json::Value(comparison.lightOccluderDistance));
        item.set("penumbraRadius", json::Value(comparison.penumbraRadius));
        item.set("penumbraWidth", json::Value(comparison.penumbraWidth));
        item.set("edgeWidth50", json::Value(comparison.edgeWidth50));
        json::Value offsets = json::Value::array();
        json::Value values = json::Value::array();
        for (std::size_t i = 0; i < comparison.sampleOffsets.size(); ++i) {
            offsets.push(json::Value(comparison.sampleOffsets[i]));
            values.push(json::Value(comparison.sampleBrightness[i]));
        }
        item.set("sampleOffsets", std::move(offsets));
        item.set("sampleBrightness", std::move(values));
        comparisons.push(std::move(item));
    }
    root.set("penumbraComparisons", std::move(comparisons));
    return root;
}

std::string RenderContext::projectionCanonical() const {
    return projectionReport().canonical();
}

std::string linearToSrgbHex(Vec3 linear, double exposure) {
    const Vec3 mapped = toneMap(linear, exposure);
    auto channel = [&](double value) {
        return static_cast<std::uint8_t>(
            std::lround(std::pow(clamp01(value), 1.0 / 2.2) * 255.0));
    };
    const std::array<std::uint8_t, 3> bytes = {channel(mapped.x), channel(mapped.y),
                                               channel(mapped.z)};
    static constexpr char kHex[] = "0123456789abcdef";
    std::string out(6, '0');
    for (std::size_t i = 0; i < 3; ++i) {
        out[i * 2] = kHex[bytes[i] >> 4u];
        out[i * 2 + 1] = kHex[bytes[i] & 0x0fu];
    }
    return out;
}

TileData RenderContext::renderTile(int blockIndex, int x, int y, int width,
                                   int height) const {
    TileData tile;
    tile.index = blockIndex;
    tile.x = x;
    tile.y = y;
    tile.width = width;
    tile.height = height;
    tile.rgba.assign(static_cast<std::size_t>(width) *
                         static_cast<std::size_t>(height) * 4u,
                     0u);
    tile.owners.assign(static_cast<std::size_t>(width) *
                           static_cast<std::size_t>(height),
                       kBackgroundOwnerCode);
    tile.stats.rays = 0;
    tile.stats.layers.resize(scene.layers.size());
    for (std::size_t i = 0; i < scene.layers.size(); ++i) {
        tile.stats.layers[i].layerIndex = scene.layers[i].index;
    }

    const Vec3 forward = normalize(scene.camera.target - scene.camera.position);
    const Vec3 right = normalize(cross(forward, scene.camera.up));
    const Vec3 trueUp = normalize(cross(right, forward));
    const double aspect = static_cast<double>(scene.render.width) /
                          static_cast<double>(scene.render.height);
    const double tanHalfFov = std::tan(radians(scene.camera.fovDeg) * 0.5);
    std::vector<std::vector<Hit>> hitsByLayer(scene.layers.size());

    auto shadeBoard = [&](const Hit& hit, const Ray& ray) -> ShadedSurface {
        ShadedSurface surface;
        surface.owner = hit.ownerCode;
        const auto textureIt = textures.byOwnerCode.find(hit.ownerCode);
        Vec3 grain{0.65, 0.55, 0.42};
        double transmittanceModulation = 0.85;
        if (textureIt != textures.byOwnerCode.end()) {
            grain = textureIt->second.sampleColor(hit.uv.x, hit.uv.y);
            transmittanceModulation =
                textureIt->second.sampleTransmittance(hit.uv.x, hit.uv.y);
        }
        Vec3 normal = hit.normal;
        if (dot(normal, ray.direction) > 0.0) normal = -normal;
        Vec3 transmitted{};
        double transmittanceSum = 0.0;
        for (std::size_t li = 0; li < scene.lights.size(); ++li) {
            const Light& light = scene.lights[li];
            const Vec3 toLight = normalize(light.position - hit.point);
            const double incidence = std::abs(dot(normal, toLight));
            const double distance = length(light.position - hit.point);
            double shadow = 1.0;
            for (const Layer& occluder : scene.layers) {
                if (occluder.z >= hit.layer->z - 1e-9) continue;
                const ShadowMap* map =
                    impl_->map(static_cast<int>(li), occluder.index, hit.layer->index);
                if (map == nullptr) continue;
                const double radius =
                    computePenumbraRadius(scene, light, occluder.z, hit.layer->z);
                const std::uint64_t seed = splitmix64(
                    static_cast<std::uint64_t>(li + 1) * 0x9e3779b97f4a7c15ull ^
                    static_cast<std::uint64_t>(occluder.index + 3) *
                        0xbf58476d1ce4e5b9ull ^
                    static_cast<std::uint64_t>(hit.layer->index + 7) *
                        0x94d049bb133111ebull);
                shadow *= map->softSample({hit.point.x, hit.point.y}, radius,
                                          scene.render.projectionSamples, seed);
            }
            const double transmittance =
                clamp01(hit.materialAmount * (0.48 + 0.52 * incidence) *
                        (0.65 + 0.35 * transmittanceModulation));
            transmittanceSum += transmittance;
            const double energy = light.intensity * incidence * shadow *
                                  lightAttenuation(distance);
            transmitted += light.color * (energy * transmittance);
        }
        const double averageTransmittance =
            clamp01(transmittanceSum /
                    static_cast<double>(std::max<std::size_t>(1, scene.lights.size())));
        const double opacity = 1.0 - std::min(0.92, averageTransmittance);
        const Vec3 ambientWood =
            grain * (scene.render.ambient * 0.82 + 0.025);
        const Vec3 transmittedWood =
            transmitted * grain * (0.62 + 0.25 * transmittanceModulation);
        surface.premultiplied = (ambientWood + transmittedWood) * opacity;
        surface.opacity = opacity;
        surface.transmit = 0.35 * averageTransmittance;
        surface.transmittedEnergy = surface.transmit;
        surface.reflectedEnergy = opacity;
        return surface;
    };

    auto shadeBracket = [&](const Hit& hit, const Ray& ray) -> ShadedSurface {
        ShadedSurface surface;
        surface.owner = hit.ownerCode;
        Vec3 normal = hit.normal;
        if (dot(normal, ray.direction) > 0.0) normal = -normal;
        const double viewCos =
            std::max(0.16, std::abs(dot(normal, ray.direction)));
        const double thicknessTransmit =
            std::exp(-58.0 * hit.layer->thickness / viewCos);
        surface.transmit = clamp01(thicknessTransmit);
        surface.opacity = 1.0 - surface.transmit;
        Vec3 reflected = hit.color * (scene.render.ambient * 0.55);
        Vec3 specular{};
        for (const Light& light : scene.lights) {
            const Vec3 toLight = normalize(light.position - hit.point);
            const double incidence = std::abs(dot(normal, toLight));
            const double distance = length(light.position - hit.point);
            const double attenuation = lightAttenuation(distance);
            reflected += hit.color * (hit.materialAmount * light.intensity * incidence *
                                      attenuation);
            const double spec = std::pow(std::max(0.0, incidence), 36.0) *
                                hit.materialAmount * 0.11 * light.intensity *
                                attenuation;
            specular += light.color * spec;
        }
        surface.premultiplied = (reflected + specular) * surface.opacity;
        surface.reflectedEnergy = surface.opacity;
        surface.transmittedEnergy = surface.transmit;
        return surface;
    };

    auto shadeReceiver = [&](const Ray& ray, Vec3 point) -> Vec3 {
        const Vec3 normal =
            ray.direction.z >= 0.0 ? Vec3{0.0, 0.0, -1.0} : Vec3{0.0, 0.0, 1.0};
        Vec3 direct{};
        for (std::size_t li = 0; li < scene.lights.size(); ++li) {
            const Light& light = scene.lights[li];
            const Vec3 toLight = normalize(light.position - point);
            const double incidence = std::abs(dot(normal, toLight));
            const double distance = length(light.position - point);
            double shadow = 1.0;
            for (const Layer& occluder : scene.layers) {
                if (occluder.z >= scene.receiver.z - 1e-9) continue;
                const ShadowMap* map =
                    impl_->map(static_cast<int>(li), occluder.index, kReceiverTargetKey);
                if (map == nullptr) continue;
                const double radius = computePenumbraRadius(
                    scene, light, occluder.z, scene.receiver.z);
                const std::uint64_t seed = splitmix64(
                    static_cast<std::uint64_t>(li + 1) * 0x9e3779b97f4a7c15ull ^
                    static_cast<std::uint64_t>(occluder.index + 11) *
                        0xbf58476d1ce4e5b9ull ^
                    0xc67178f2a4506cebull);
                shadow *= map->softSample({point.x, point.y}, radius,
                                          scene.render.projectionSamples, seed);
            }
            const double energy =
                light.intensity * incidence * shadow * lightAttenuation(distance);
            direct += light.color * energy;
        }
        const Vec3 ambient{scene.render.ambient * 0.85,
                           scene.render.ambient * 0.85,
                           scene.render.ambient * 0.85};
        return scene.receiver.color * (ambient + direct * 0.92);
    };

    const int imageWidth = scene.render.width;
    const int imageHeight = scene.render.height;
    for (int py = 0; py < height; ++py) {
        for (int px = 0; px < width; ++px) {
            const int imageX = x + px;
            const int imageY = y + py;
            const double u =
                (static_cast<double>(imageX) + 0.5) /
                    static_cast<double>(imageWidth) * 2.0 - 1.0;
            const double v =
                1.0 - (static_cast<double>(imageY) + 0.5) /
                          static_cast<double>(imageHeight) * 2.0;
            Ray ray;
            ray.origin = scene.camera.position;
            ray.direction = normalize(forward + right * (u * tanHalfFov * aspect) +
                                      trueUp * (v * tanHalfFov));
            for (auto& list : hitsByLayer) list.clear();

            for (std::size_t layerIndex = 0; layerIndex < scene.layers.size();
                 ++layerIndex) {
                const Layer& layer = scene.layers[layerIndex];
                for (const Board& board : layer.boards) {
                    for (std::size_t triangleIndex = 0;
                         triangleIndex < board.mesh.triangles.size();
                         ++triangleIndex) {
                        const Triangle& tri = board.mesh.triangles[triangleIndex];
                        double t = 0.0;
                        Barycentric bary;
                        if (!intersectTriangle(ray, board.mesh, tri, scene.camera.nearZ,
                                               scene.camera.farZ, t, bary)) {
                            continue;
                        }
                        Hit hit;
                        hit.mesh = &board.mesh;
                        hit.layer = &layer;
                        hit.type = ComponentType::Board;
                        hit.ownerCode = board.ownerCode;
                        hit.triangleIndex = static_cast<std::int64_t>(triangleIndex);
                        hit.t = t;
                        hit.point = ray.origin + ray.direction * t;
                        hit.uv = interpolate(tri.uv[0], tri.uv[1], tri.uv[2], bary);
                        hit.normal = triangleNormal(board.mesh, tri);
                        hit.materialAmount = board.transmittance;
                        hit.color = board.color;
                        hitsByLayer[layerIndex].push_back(hit);
                    }
                }
                for (const Bracket& bracket : layer.brackets) {
                    for (std::size_t triangleIndex = 0;
                         triangleIndex < bracket.mesh.triangles.size();
                         ++triangleIndex) {
                        const Triangle& tri = bracket.mesh.triangles[triangleIndex];
                        double t = 0.0;
                        Barycentric bary;
                        if (!intersectTriangle(ray, bracket.mesh, tri, scene.camera.nearZ,
                                               scene.camera.farZ, t, bary)) {
                            continue;
                        }
                        Hit hit;
                        hit.mesh = &bracket.mesh;
                        hit.layer = &layer;
                        hit.type = ComponentType::Bracket;
                        hit.ownerCode = bracket.ownerCode;
                        hit.triangleIndex = static_cast<std::int64_t>(triangleIndex);
                        hit.t = t;
                        hit.point = ray.origin + ray.direction * t;
                        hit.uv = interpolate(tri.uv[0], tri.uv[1], tri.uv[2], bary);
                        hit.normal = triangleNormal(bracket.mesh, tri);
                        hit.componentThickness = bracket.thickness;
                        hit.materialAmount = bracket.reflectance;
                        hit.color = bracket.color;
                        hitsByLayer[layerIndex].push_back(hit);
                    }
                }
            }

            Vec3 premultipliedColor{};
            double alpha = 0.0;
            double visibility = 1.0;
            int owner = kBackgroundOwnerCode;
            bool ownerAssigned = false;
            for (std::size_t layerIndex = 0; layerIndex < hitsByLayer.size();
                 ++layerIndex) {
                std::vector<Hit>& hits = hitsByLayer[layerIndex];
                if (hits.empty()) continue;
                std::sort(hits.begin(), hits.end(), [](const Hit& a, const Hit& b) {
                    auto quantize = [](double value) {
                        return static_cast<std::int64_t>(
                            std::llround(value * 1.0e10));
                    };
                    const std::int64_t at = quantize(a.t);
                    const std::int64_t bt = quantize(b.t);
                    if (at != bt) return at < bt;
                    const std::int64_t ax = quantize(a.point.x);
                    const std::int64_t bx = quantize(b.point.x);
                    if (ax != bx) return ax < bx;
                    const std::int64_t ay = quantize(a.point.y);
                    const std::int64_t by = quantize(b.point.y);
                    if (ay != by) return ay < by;
                    const std::int64_t az = quantize(a.point.z);
                    const std::int64_t bz = quantize(b.point.z);
                    if (az != bz) return az < bz;
                    if (a.type != b.type) return a.type < b.type;
                    if (a.ownerCode != b.ownerCode) {
                        return a.ownerCode < b.ownerCode;
                    }
                    return a.triangleIndex < b.triangleIndex;
                });
                double minT = std::numeric_limits<double>::infinity();
                for (const Hit& hit : hits) minT = std::min(minT, hit.t);
                const Hit* nearest = nullptr;
                for (const Hit& hit : hits) {
                    if (std::abs(hit.t - minT) <= 1e-12 &&
                        (nearest == nullptr || hit.ownerCode < nearest->ownerCode)) {
                        nearest = &hit;
                    }
                }
                if (nearest == nullptr) nearest = &hits.front();
                double maxThickness = scene.layers[layerIndex].thickness;
                for (const Hit& hit : hits) {
                    maxThickness = std::max(maxThickness, hit.componentThickness);
                }
                const double incidence =
                    std::max(0.12, std::abs(dot(ray.direction, nearest->normal)));
                const double overlapScale = maxThickness / incidence;
                const double cutoff = overlapScale * 6.0;

                ShadedSurface blended;
                double sumWeights = 0.0;
                double maxOwnerWeight = -1.0;
                int layerOwner = kBackgroundOwnerCode;
                for (const Hit& hit : hits) {
                    const double delta = hit.t - minT;
                    if (delta > cutoff) continue;
                    const double weight = std::exp(-delta / overlapScale);
                    const ShadedSurface shaded =
                        hit.type == ComponentType::Board ? shadeBoard(hit, ray)
                                                         : shadeBracket(hit, ray);
                    blended.premultiplied += shaded.premultiplied * weight;
                    blended.opacity += shaded.opacity * weight;
                    blended.transmit += shaded.transmit * weight;
                    blended.reflectedEnergy += shaded.reflectedEnergy * weight;
                    blended.transmittedEnergy += shaded.transmittedEnergy * weight;
                    sumWeights += weight;
                    if (weight > maxOwnerWeight + 1e-14 ||
                        (std::abs(weight - maxOwnerWeight) <= 1e-14 &&
                         shaded.owner < layerOwner)) {
                        maxOwnerWeight = weight;
                        layerOwner = shaded.owner;
                    }
                }
                if (!(sumWeights > 0.0)) continue;
                blended.premultiplied = blended.premultiplied / sumWeights;
                blended.opacity /= sumWeights;
                blended.transmit /= sumWeights;
                blended.reflectedEnergy /= sumWeights;
                blended.transmittedEnergy /= sumWeights;
                blended.owner = layerOwner;

                premultipliedColor += blended.premultiplied * visibility;
                alpha += blended.opacity * visibility;
                LayerEnergyStats& stats = tile.stats.layers[layerIndex];
                stats.reflected += visibility * blended.reflectedEnergy;
                stats.transmitted += visibility * blended.transmittedEnergy;
                ++stats.samples;
                if (!ownerAssigned) {
                    ownerAssigned = true;
                    owner = blended.owner;
                }
                visibility *= blended.transmit;
                if (visibility < 1e-12) {
                    visibility = 0.0;
                    break;
                }
            }

            if (visibility > 0.0) {
                double receiverT = 0.0;
                if (std::abs(ray.direction.z) > 1e-12) {
                    receiverT =
                        (scene.receiver.z - ray.origin.z) / ray.direction.z;
                }
                if (receiverT > scene.camera.nearZ &&
                    receiverT < scene.camera.farZ) {
                    const Vec3 point = ray.origin + ray.direction * receiverT;
                    if (point.x >= scene.receiver.minX &&
                        point.x <= scene.receiver.maxX &&
                        point.y >= scene.receiver.minY &&
                        point.y <= scene.receiver.maxY) {
                        const Vec3 color = shadeReceiver(ray, point);
                        premultipliedColor += color * visibility;
                        alpha += visibility;
                        if (!ownerAssigned) {
                            ownerAssigned = true;
                            owner = kReceiverOwnerCode;
                        }
                        visibility = 0.0;
                    }
                }
            }
            if (visibility > 0.0) {
                premultipliedColor += scene.render.background * visibility;
                if (!ownerAssigned) {
                    owner = kBackgroundOwnerCode;
                }
                alpha += visibility;
                visibility = 0.0;
            }

            const double clampedAlpha = clamp01(alpha);
            Vec3 straight{};
            if (clampedAlpha > 1e-12) {
                straight = premultipliedColor / clampedAlpha;
            }
            const Vec3 mapped = toneMap(straight, scene.render.exposure);
            const Vec3 finalPremul = mapped * clampedAlpha;
            auto gammaByte = [](double value) {
                return toByte(std::pow(clamp01(value), 1.0 / 2.2));
            };
            const std::size_t pixel =
                static_cast<std::size_t>(py) * static_cast<std::size_t>(width) +
                static_cast<std::size_t>(px);
            tile.rgba[pixel * 4] = gammaByte(finalPremul.x);
            tile.rgba[pixel * 4 + 1] = gammaByte(finalPremul.y);
            tile.rgba[pixel * 4 + 2] = gammaByte(finalPremul.z);
            tile.rgba[pixel * 4 + 3] = toByte(clampedAlpha);
            tile.owners[pixel] = owner;
            ++tile.stats.rays;
        }
    }
    return tile;
}

}  // namespace cr

