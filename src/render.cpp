#include "render.hpp"

#include <algorithm>
#include <cmath>
#include <filesystem>
#include <fstream>
#include <limits>
#include <sstream>
#include <stdexcept>

namespace sl {
namespace {

Vec3 toneMap(const Vec3& value) {
    const Vec3 nonNegative{
        std::max(0.0, value.x),
        std::max(0.0, value.y),
        std::max(0.0, value.z),
    };
    return {
        nonNegative.x / (1.0 + nonNegative.x),
        nonNegative.y / (1.0 + nonNegative.y),
        nonNegative.z / (1.0 + nonNegative.z),
    };
}

std::uint8_t encodeChannel(double linear, double alpha, double exposure) {
    const double straight = alpha > 1.0e-9 ? linear / alpha : 0.0;
    const Vec3 mapped = toneMap(Vec3(straight * exposure, 0.0, 0.0));
    const double gammaEncoded = std::pow(clamp(mapped.x, 0.0, 1.0), 1.0 / 2.2);
    return static_cast<std::uint8_t>(std::llround(clamp(gammaEncoded * alpha, 0.0, 1.0) * 255.0));
}

Vec3 lightTintAverage(const Scene& scene) {
    Vec3 sum;
    double total = 0.0;
    for (const StripLight& light : scene.lights) {
        sum += componentMul(light.color, Vec3(light.intensity, light.intensity, light.intensity));
        total += light.intensity;
    }
    if (total <= kEpsilon) return {1.0, 1.0, 1.0};
    const Vec3 average = sum / total;
    return clampColor(average);
}

double percentile(std::vector<double> values, double fraction) {
    if (values.empty()) return 0.0;
    std::sort(values.begin(), values.end());
    const double position = clamp(fraction, 0.0, 1.0) * static_cast<double>(values.size() - 1);
    const std::size_t low = static_cast<std::size_t>(std::floor(position));
    const std::size_t high = static_cast<std::size_t>(std::ceil(position));
    const double t = position - static_cast<double>(low);
    return values[low] * (1.0 - t) + values[high] * t;
}

Vec2 boardUV(const Scene& scene, const Vec3& point) {
    const Vec3 local = point - scene.backboard.origin;
    return {dot(local, scene.backboard.u), dot(local, scene.backboard.v)};
}

bool insideBoard(const Scene& scene, const Vec3& point) {
    return pointInPlaneRect(scene.backboard, point);
}

int nextOuterLayerSlot(const std::vector<ScreenLayer>& layers, int slot) {
    if (slot + 1 < static_cast<int>(layers.size())) return slot + 1;
    return -1;
}

} // namespace

Renderer::Renderer(const Scene& scene) : scene_(scene) {
    camera_.position = scene.camera.position;
    camera_.forward = normalize(scene.camera.target - scene.camera.position);
    camera_.right = normalize(cross(camera_.forward, scene.camera.up));
    camera_.up = normalize(cross(camera_.right, camera_.forward));
    camera_.tanHalfFov = std::tan(radians(scene.camera.fovYDegrees) * 0.5);
    camera_.aspect = static_cast<double>(scene.image.width) /
                     static_cast<double>(scene.image.height);
    buildBlocks();
    buildMoirePairs();
    buildProjectionPoints();
}

void Renderer::buildBlocks() {
    blocks_.clear();
    const int blockW = scene_.image.blockWidth;
    const int blockH = scene_.image.blockHeight;
    int id = 0;
    for (int y = 0; y < scene_.image.height; y += blockH) {
        for (int x = 0; x < scene_.image.width; x += blockW) {
            BlockRect block;
            block.id = id++;
            block.x = x;
            block.y = y;
            block.width = std::min(blockW, scene_.image.width - x);
            block.height = std::min(blockH, scene_.image.height - y);
            blocks_.push_back(block);
        }
    }
}

BlockRect Renderer::blockRect(int id) const {
    if (id < 0 || id >= static_cast<int>(blocks_.size())) {
        throw std::out_of_range("block id out of range");
    }
    return blocks_[static_cast<std::size_t>(id)];
}

Ray Renderer::pixelRay(int x, int y) const {
    const double ndcX =
        (2.0 * (static_cast<double>(x) + 0.5) / static_cast<double>(scene_.image.width) - 1.0) *
        camera_.aspect * camera_.tanHalfFov;
    const double ndcY =
        (1.0 - 2.0 * (static_cast<double>(y) + 0.5) / static_cast<double>(scene_.image.height)) *
        camera_.tanHalfFov;
    Ray ray;
    ray.origin = camera_.position;
    ray.direction = normalize(camera_.forward + camera_.right * ndcX + camera_.up * ndcY);
    return ray;
}

bool Renderer::meshContains(const ScreenLayer& layer, const Vec3& point) const {
    for (const Triangle& triangle : layer.triangleData) {
        if (pointOnTriangle(point, triangle, 2.0e-6)) return true;
    }
    return false;
}

Vec3 Renderer::layerFilterColor(const ScreenLayer& layer, double edge) const {
    const std::uint64_t saltHash = hashString(layer.separationId);
    const Vec3 salt{
        (static_cast<double>(saltHash & 0xFFULL) / 255.0 - 0.5) * 0.055,
        (static_cast<double>((saltHash >> 8u) & 0xFFULL) / 255.0 - 0.5) * 0.055,
        (static_cast<double>((saltHash >> 16u) & 0xFFULL) / 255.0 - 0.5) * 0.055,
    };
    const Vec3 edgeScale{
        0.91 + 0.09 * edge,
        0.91 + 0.09 * edge,
        0.91 + 0.09 * edge,
    };
    return clampColor(componentMul(layer.inkColor, edgeScale) + salt);
}

double Renderer::layerAperture(const ScreenLayer& layer,
                               const Vec3& point,
                               double tau,
                               double filterWeight) const {
    if (!meshContains(layer, point)) return 1.0;
    const double coverage = sampleLayerCoverage(scene_, layer, point, filterWeight);
    return clamp(1.0 - coverage * (1.0 - tau), 0.0, 1.0);
}

Vec3 Renderer::transmissionAlongRay(const Vec3& origin, const Vec3& target) const {
    Vec3 transmission{1.0, 1.0, 1.0};
    std::vector<double> apertures(scene_.layers.size(), 1.0);
    const Vec3 delta = target - origin;
    for (std::size_t layerSlot = 0; layerSlot < scene_.layers.size(); ++layerSlot) {
        const ScreenLayer& layer = scene_.layers[layerSlot];
        const double denom = dot(delta, layer.plane.normal);
        if (std::fabs(denom) < 1.0e-12) continue;
        const double t = dot(layer.plane.origin - origin, layer.plane.normal) / denom;
        if (t <= 1.0e-5 || t >= 1.0 - 1.0e-5) continue;
        const Vec3 point = origin + delta * t;
        if (!meshContains(layer, point)) continue;

        const double viewCos = clamp(std::fabs(dot(layer.plane.normal, normalize(delta))),
                                     0.12, 1.0);
        const double tauAngle =
            std::pow(clamp(layer.inkTransmittance, 0.0, 1.0), 1.0 / viewCos);
        const HalftoneSample sample = layer.halftone.sampleWorld(
            point, layer.plane.origin, layer.plane.u, layer.plane.v,
            std::max(scene_.backboard.width, 1.0), 0.035);
        const double coverage = sample.coverage(0.035);
        const Vec3 filterColor = layerFilterColor(layer, sample.edge);
        const Vec3 filter = componentMul(
            Vec3(tauAngle, tauAngle, tauAngle), filterColor);
        const Vec3 layerT = Vec3(1.0, 1.0, 1.0) * 0.0 -
                            (Vec3(1.0, 1.0, 1.0) - filter) * coverage;
        transmission = componentMul(transmission, Vec3(1.0, 1.0, 1.0) + layerT);
        apertures[layerSlot] =
            clamp(1.0 - coverage * (1.0 - tauAngle), 0.0, 1.0);
    }

    for (const MoirePairReport& pair : moirePairs_) {
        if (!pair.triggered) continue;
        int slotA = -1;
        int slotB = -1;
        for (std::size_t i = 0; i < scene_.layers.size(); ++i) {
            if (scene_.layers[i].layerIndex == pair.layerA) slotA = static_cast<int>(i);
            if (scene_.layers[i].layerIndex == pair.layerB) slotB = static_cast<int>(i);
        }
        if (slotA < 0 || slotB < 0) continue;
        const double product = apertures[static_cast<std::size_t>(slotA)] *
                               apertures[static_cast<std::size_t>(slotB)];
        const double targetProduct = pairMeanApertureA_[static_cast<std::size_t>(slotA)] *
                                     pairMeanApertureB_[static_cast<std::size_t>(slotB)];
        const double masked = product + pair.weight * (targetProduct - product);
        if (product > 1.0e-8) {
            const double factor = clamp(masked / product, 0.0, 3.0);
            transmission *= factor;
        }
    }
    return clampColor(transmission);
}

Vec3 Renderer::illuminateBoard(const Vec3& point) const {
    Vec3 radiance{0.014, 0.016, 0.023};
    for (const StripLight& light : scene_.lights) {
        if (light.intensity <= 0.0) continue;
        const Vec3 lightPoint = closestPointOnSegment(point, light.a, light.b);
        const Vec3 toLight = lightPoint - point;
        const double distance = length(toLight);
        if (distance <= kEpsilon) continue;
        const Vec3 dir = toLight / distance;
        const double cosine = clamp(std::fabs(dot(scene_.backboard.normal, dir)), 0.0, 1.0);
        const double falloff = 1.0 / (1.0 + (distance / 115.0) * (distance / 115.0));
        const Vec3 shadow = transmissionAlongRay(point, lightPoint);
        const double lightAmount = light.intensity * falloff * (0.12 + 0.88 * cosine);
        radiance += componentMul(light.color, shadow) * lightAmount;
    }
    return radiance;
}

double Renderer::directLight(const Vec3& point, const Vec3& normal, Vec3& tint) const {
    double amount = 0.0;
    Vec3 accumulated{0.0, 0.0, 0.0};
    for (const StripLight& light : scene_.lights) {
        if (light.intensity <= 0.0) continue;
        const Vec3 lightPoint = closestPointOnSegment(point, light.a, light.b);
        const Vec3 toLight = lightPoint - point;
        const double distance = length(toLight);
        if (distance <= kEpsilon) continue;
        const Vec3 dir = toLight / distance;
        const double cosine = clamp(std::fabs(dot(normal, dir)), 0.0, 1.0);
        const double falloff = 1.0 / (1.0 + (distance / 145.0) * (distance / 145.0));
        const double contribution = light.intensity * falloff * (0.22 + 0.78 * cosine);
        amount += contribution;
        accumulated += componentMul(light.color,
                                    Vec3(contribution, contribution, contribution));
    }
    if (amount <= kEpsilon) {
        tint = {1.0, 1.0, 1.0};
    } else {
        tint = clampColor(accumulated / amount);
    }
    return amount;
}

double Renderer::filterWeightFor(const ScreenLayer& layer,
                                 const Hit& hit,
                                 int hitCount) const {
    const double worldPerPixel =
        2.0 * camera_.tanHalfFov * hit.t / static_cast<double>(scene_.image.height);
    const double spacing =
        std::max(scene_.backboard.width / static_cast<double>(layer.meshLineCount), 1.0e-6);
    double weight = clamp(worldPerPixel / spacing * 0.8, 0.0, 0.92);
    weight = std::max(weight, clamp(layer.airGapAbove / 50.0, 0.0, 0.18));
    if (hitCount > 1) weight = std::max(weight, 0.44);
    return weight;
}

void Renderer::renderPixel(int globalX,
                           int globalY,
                           int localX,
                           int localY,
                           std::vector<std::uint8_t>& rgba,
                           std::vector<std::uint8_t>& owner,
                           std::vector<LayerMetric>& metrics,
                           int localWidth) const {
    const Ray ray = pixelRay(globalX, globalY);
    Hit hits[4];
    int hitCount = 0;

    for (std::size_t layerSlot = 0; layerSlot < scene_.layers.size(); ++layerSlot) {
        const ScreenLayer& layer = scene_.layers[layerSlot];
        Hit best;
        best.layerSlot = static_cast<int>(layerSlot);
        for (const Triangle& triangle : layer.triangleData) {
            double t = 0.0;
            double u = 0.0;
            double v = 0.0;
            if (rayTriangle(ray, triangle, t, u, v) &&
                t >= scene_.camera.nearPlane && t <= scene_.camera.farPlane &&
                (!best.valid || t < best.t)) {
                best.valid = true;
                best.t = t;
                best.point = ray.origin + ray.direction * t;
                best.normal = triangle.normal;
            }
        }
        if (best.valid && hitCount < 4) {
            hits[hitCount++] = best;
        }
    }

    double boardT = 0.0;
    bool boardHit = false;
    Vec3 boardPoint;
    if (intersectPlane(ray, scene_.backboard, boardT) && boardT >= scene_.camera.nearPlane &&
        boardT <= scene_.camera.farPlane && pointInPlaneRect(scene_.backboard,
                                                             ray.origin + ray.direction * boardT)) {
        boardHit = true;
        boardPoint = ray.origin + ray.direction * boardT;
    }

    Vec3 radiance = boardHit ? illuminateBoard(boardPoint) : Vec3();
    double alpha = boardHit ? 1.0 : 0.0;
    int pixelOwner = 0;

    const Vec3 ambientTint = lightTintAverage(scene_);
    for (int hitIndex = 0; hitIndex < hitCount; ++hitIndex) {
        const Hit& hit = hits[hitIndex];
        const ScreenLayer& layer = scene_.layers[static_cast<std::size_t>(hit.layerSlot)];
        Vec3 normal = hit.normal;
        const double viewCos = clamp(std::fabs(dot(normal, ray.direction)), 0.12, 1.0);
        const double filterWeight = filterWeightFor(layer, hit, hitCount);
        const HalftoneSample sample = layer.halftone.sampleWorld(
            hit.point, layer.plane.origin, layer.plane.u, layer.plane.v,
            std::max(scene_.backboard.width, 1.0), filterWeight);
        const double coverage = sample.coverage(filterWeight);
        const Vec3 filterColor = layerFilterColor(layer, sample.edge);
        const double tauAngle =
            std::pow(clamp(layer.inkTransmittance, 0.0, 1.0), 1.0 / viewCos);
        const Vec3 inkFilter = componentMul(
            Vec3(tauAngle, tauAngle, tauAngle), filterColor);
        const Vec3 transmittance = Vec3(1.0, 1.0, 1.0) -
                                   (Vec3(1.0, 1.0, 1.0) - inkFilter) * coverage;

        Vec3 tint;
        const double irradiance = directLight(hit.point, normal, tint);
        const double fresnel = 0.032 + 0.21 * std::pow(1.0 - viewCos, 5.0);
        const double reflectedAmount = irradiance * fresnel * 0.22;
        const Vec3 reflection =
            componentMul(tint, Vec3(reflectedAmount, reflectedAmount, reflectedAmount)) +
            componentMul(ambientTint * 0.012, filterColor);
        radiance = componentMul(radiance, transmittance) + reflection;

        const double alphaLayer = clamp((1.0 - luminance(transmittance)) * 0.72 +
                                            fresnel * 0.08,
                                        0.0, 0.92);
        alpha = alphaLayer + (1.0 - alphaLayer) * alpha;
        if (coverage > 0.018 || alphaLayer > 0.012) {
            pixelOwner = layer.layerIndex;
        }

        LayerMetric& metric = metrics[static_cast<std::size_t>(hit.layerSlot)];
        metric.transmitted += luminance(transmittance);
        metric.reflected += luminance(reflection);
        metric.coverage += coverage;
        metric.samples += 1.0;
    }

    const std::size_t pixel = (static_cast<std::size_t>(localY) *
                                   static_cast<std::size_t>(localWidth) +
                               static_cast<std::size_t>(localX)) *
                              4u;
    const double exposure = scene_.image.exposure;
    rgba[pixel + 0] = encodeChannel(radiance.x, alpha, exposure);
    rgba[pixel + 1] = encodeChannel(radiance.y, alpha, exposure);
    rgba[pixel + 2] = encodeChannel(radiance.z, alpha, exposure);
    rgba[pixel + 3] = static_cast<std::uint8_t>(
        std::llround(clamp(alpha, 0.0, 1.0) * 255.0));
    owner[static_cast<std::size_t>(localY) * static_cast<std::size_t>(localWidth) +
          static_cast<std::size_t>(localX)] = static_cast<std::uint8_t>(pixelOwner);
}

BlockResult Renderer::renderBlock(int id) const {
    const BlockRect rect = blockRect(id);
    BlockResult result;
    result.rect = rect;
    result.rgba.assign(static_cast<std::size_t>(rect.width * rect.height * 4), 0);
    result.owner.assign(static_cast<std::size_t>(rect.width * rect.height), 0);
    result.metrics.resize(scene_.layers.size());
    for (std::size_t i = 0; i < scene_.layers.size(); ++i) {
        result.metrics[i].layerIndex = scene_.layers[i].layerIndex;
        result.metrics[i].separationId = scene_.layers[i].separationId;
    }

    for (int localY = 0; localY < rect.height; ++localY) {
        for (int localX = 0; localX < rect.width; ++localX) {
            renderPixel(rect.x + localX, rect.y + localY, localX, localY,
                        result.rgba, result.owner, result.metrics, rect.width);
        }
    }

    result.rgbaDigest = rgbaDigest(result.rgba);

    std::uint64_t ownerHash = 1469598103934665603ULL;
    for (std::uint8_t value : result.owner) {
        ownerHash ^= value;
        ownerHash *= 1099511628211ULL;
    }
    result.ownerDigest = hex64(hashMix(ownerHash));
    return result;
}

std::string Renderer::rgbaDigest(const std::vector<std::uint8_t>& bytes) {
    std::uint64_t hash = 1469598103934665603ULL;
    for (std::uint8_t value : bytes) {
        hash ^= value;
        hash *= 1099511628211ULL;
    }
    return hex64(hashMix(hash));
}

std::vector<LayerMetric> Renderer::combineMetrics(const std::vector<LayerMetric>& a,
                                                  const std::vector<LayerMetric>& b) {
    std::vector<LayerMetric> result = a;
    if (result.size() < b.size()) result.resize(b.size());
    for (std::size_t i = 0; i < b.size(); ++i) {
        if (result[i].separationId.empty()) {
            result[i].layerIndex = b[i].layerIndex;
            result[i].separationId = b[i].separationId;
        }
        result[i].transmitted += b[i].transmitted;
        result[i].reflected += b[i].reflected;
        result[i].coverage += b[i].coverage;
        result[i].samples += b[i].samples;
    }
    return result;
}

double Renderer::pairAmplitude(int layerA,
                               int layerB,
                               int lightIndex,
                               bool applyMask,
                               double weight) const {
    const int slotA = layerA;
    const int slotB = layerB;
    if (slotA < 0 || slotB < 0 || slotA >= static_cast<int>(scene_.layers.size()) ||
        slotB >= static_cast<int>(scene_.layers.size())) {
        return 0.0;
    }
    const ScreenLayer& a = scene_.layers[static_cast<std::size_t>(slotA)];
    const ScreenLayer& b = scene_.layers[static_cast<std::size_t>(slotB)];
    const StripLight& light = scene_.lights[static_cast<std::size_t>(lightIndex)];
    const int grid = 128;
    const int coarseFactor = 8;
    const int coarse = grid / coarseFactor;
    std::vector<double> values(static_cast<std::size_t>(grid * grid), 0.0);
    std::vector<double> coarseValues(static_cast<std::size_t>(coarse * coarse), 0.0);
    double meanA = 0.0;
    double meanB = 0.0;

    for (int gy = 0; gy < grid; ++gy) {
        for (int gx = 0; gx < grid; ++gx) {
            const double u = ((static_cast<double>(gx) + 0.5) / grid - 0.5) *
                             scene_.backboard.width;
            const double v = ((static_cast<double>(gy) + 0.5) / grid - 0.5) *
                             scene_.backboard.height;
            const Vec3 boardPoint = scene_.backboard.origin +
                                    scene_.backboard.u * u +
                                    scene_.backboard.v * v;
            const Vec3 lightPoint =
                closestPointOnSegment(boardPoint, light.a, light.b);
            const Vec3 delta = lightPoint - boardPoint;

            const auto aperture = [&](const ScreenLayer& layer) {
                const double denom = dot(delta, layer.plane.normal);
                if (std::fabs(denom) < 1.0e-12) return 1.0;
                const double t =
                    dot(layer.plane.origin - boardPoint, layer.plane.normal) / denom;
                if (t <= 1.0e-5 || t >= 1.0 - 1.0e-5) return 1.0;
                const Vec3 point = boardPoint + delta * t;
                if (!meshContains(layer, point)) return 1.0;
                const HalftoneSample sample = layer.halftone.sampleWorld(
                    point, layer.plane.origin, layer.plane.u, layer.plane.v,
                    std::max(scene_.backboard.width, 1.0), 0.0);
                const double coverage = sample.coverage(0.0);
                return clamp(1.0 - coverage * (1.0 - layer.inkTransmittance), 0.0, 1.0);
            };

            const double av = aperture(a);
            const double bv = aperture(b);
            meanA += av;
            meanB += bv;
            values[static_cast<std::size_t>(gy * grid + gx)] = av * bv;
        }
    }
    meanA /= static_cast<double>(grid * grid);
    meanB /= static_cast<double>(grid * grid);
    const double target = meanA * meanB;
    for (int cy = 0; cy < coarse; ++cy) {
        for (int cx = 0; cx < coarse; ++cx) {
            double sum = 0.0;
            for (int oy = 0; oy < coarseFactor; ++oy) {
                for (int ox = 0; ox < coarseFactor; ++ox) {
                    const int gx = cx * coarseFactor + ox;
                    const int gy = cy * coarseFactor + oy;
                    double value = values[static_cast<std::size_t>(gy * grid + gx)];
                    if (applyMask) value += weight * (target - value);
                    sum += value;
                }
            }
            coarseValues[static_cast<std::size_t>(cy * coarse + cx)] =
                sum / static_cast<double>(coarseFactor * coarseFactor);
        }
    }
    const double low = percentile(coarseValues, 0.05);
    const double high = percentile(coarseValues, 0.95);
    return high - low;
}

void Renderer::buildMoirePairs() {
    moirePairs_.clear();
    pairMeanApertureA_.assign(scene_.layers.size(), 1.0);
    pairMeanApertureB_.assign(scene_.layers.size(), 1.0);
    for (std::size_t i = 0; i < scene_.layers.size(); ++i) {
        pairMeanApertureA_[i] = scene_.layers[i].halftone.meanAperture();
        pairMeanApertureB_[i] = scene_.layers[i].halftone.meanAperture();
    }
    for (std::size_t i = 0; i + 1 < scene_.layers.size(); ++i) {
        MoirePairReport report;
        report.layerA = scene_.layers[i].layerIndex;
        report.layerB = scene_.layers[i + 1].layerIndex;
        report.separationA = scene_.layers[i].separationId;
        report.separationB = scene_.layers[i + 1].separationId;
        report.angleA = scene_.layers[i].meshAngleDegrees;
        report.angleB = scene_.layers[i + 1].meshAngleDegrees;
        report.angleDifference = angleDifferenceDegrees(report.angleA, report.angleB);
        report.threshold = scene_.moire.thresholdDegrees;
        report.dotCoverageA = scene_.layers[i].halftone.meanCoverage();
        report.dotCoverageB = scene_.layers[i + 1].halftone.meanCoverage();
        report.triggered = report.threshold > 0.0 &&
                           report.angleDifference < report.threshold &&
                           scene_.moire.maskStrength > 0.0;
        if (report.triggered) {
            report.weight = scene_.moire.maskStrength *
                            (1.0 - report.angleDifference / report.threshold);
            report.weight = clamp(report.weight, 0.0, 1.0);
        }
        double beforeSum = 0.0;
        double afterSum = 0.0;
        for (int light = 0; light < 2; ++light) {
            const double before =
                pairAmplitude(static_cast<int>(i), static_cast<int>(i + 1), light, false, 0.0);
            const double after = report.triggered
                                     ? pairAmplitude(static_cast<int>(i),
                                                     static_cast<int>(i + 1),
                                                     light, true, report.weight)
                                     : before;
            report.amplitudeBeforeByLight[static_cast<std::size_t>(light)] = before;
            report.amplitudeAfterByLight[static_cast<std::size_t>(light)] = after;
            beforeSum += before;
            afterSum += after;
        }
        report.amplitudeBefore = beforeSum * 0.5;
        report.amplitudeAfter = afterSum * 0.5;
        report.reductionPercent = report.amplitudeBefore > 1.0e-12
                                      ? (report.amplitudeBefore - report.amplitudeAfter) /
                                            report.amplitudeBefore * 100.0
                                      : 0.0;
        moirePairs_.push_back(report);
    }
}

void Renderer::buildProjectionPoints() {
    projections_.clear();
    for (std::size_t layerSlot = 0; layerSlot < scene_.layers.size(); ++layerSlot) {
        const ScreenLayer& layer = scene_.layers[layerSlot];
        struct Hole {
            Vec2 uv;
            Vec3 world;
        };
        std::vector<Hole> holes;
        const int grid = 17;
        for (int gy = 0; gy < grid; ++gy) {
            for (int gx = 0; gx < grid; ++gx) {
                const double u = layer.minU +
                                 (layer.maxU - layer.minU) *
                                     (static_cast<double>(gx) + 0.5) / grid;
                const double v = layer.minV +
                                 (layer.maxV - layer.minV) *
                                     (static_cast<double>(gy) + 0.5) / grid;
                const Vec3 world = layer.plane.origin + layer.plane.u * u +
                                   layer.plane.v * v;
                if (!meshContains(layer, world)) continue;
                const HalftoneSample sample = layer.halftone.sampleWorld(
                    world, layer.plane.origin, layer.plane.u, layer.plane.v,
                    std::max(scene_.backboard.width, 1.0), 0.0);
                if (sample.coverage(0.0) < 0.34) {
                    holes.push_back({Vec2(u, v), world});
                }
            }
        }
        if (holes.empty()) {
            holes.push_back({Vec2((layer.minU + layer.maxU) * 0.5,
                                  (layer.minV + layer.maxV) * 0.5),
                             layer.plane.origin +
                                 layer.plane.u * ((layer.minU + layer.maxU) * 0.5) +
                                 layer.plane.v * ((layer.minV + layer.maxV) * 0.5)});
        }

        const int maxPoints = 16;
        std::vector<Hole> selected;
        if (static_cast<int>(holes.size()) <= maxPoints) {
            selected = holes;
        } else {
            for (int i = 0; i < maxPoints; ++i) {
                const std::size_t index =
                    static_cast<std::size_t>(i) * holes.size() /
                    static_cast<std::size_t>(maxPoints);
                selected.push_back(holes[index]);
            }
        }

        const int adjacentSlot = nextOuterLayerSlot(scene_.layers,
                                                    static_cast<int>(layerSlot));
        for (int lightIndex = 0; lightIndex < 2; ++lightIndex) {
            const StripLight& light = scene_.lights[static_cast<std::size_t>(lightIndex)];
            for (const Hole& hole : selected) {
                const Vec3 lightPoint =
                    closestPointOnSegment(hole.world, light.a, light.b);
                const Vec3 direction = hole.world - lightPoint;
                if (length(direction) <= kEpsilon) continue;
                Ray ray{hole.world, normalize(direction)};

                ProjectionPoint point;
                point.layerIndex = layer.layerIndex;
                point.separationId = layer.separationId;
                point.lightIndex = lightIndex;
                point.holeUV = hole.uv;
                point.holeWorld = hole.world;

                double boardT = 0.0;
                if (intersectPlaneBothDirections(ray, scene_.backboard, boardT) &&
                    boardT > 0.0) {
                    point.boardPoint = hole.world + ray.direction * boardT;
                    point.boardUV = boardUV(scene_, point.boardPoint);
                    point.insideBackboard = insideBoard(scene_, point.boardPoint);
                } else {
                    point.boardPoint = hole.world;
                    point.boardUV = boardUV(scene_, point.boardPoint);
                    point.insideBackboard = false;
                }

                if (adjacentSlot >= 0) {
                    const ScreenLayer& adjacent =
                        scene_.layers[static_cast<std::size_t>(adjacentSlot)];
                    double adjacentT = 0.0;
                    if (intersectPlaneBothDirections(ray, adjacent.plane, adjacentT) &&
                        adjacentT > 0.0) {
                        const Vec3 hit = hole.world + ray.direction * adjacentT;
                        if (meshContains(adjacent, hit)) {
                            const Vec3 local = hit - adjacent.plane.origin;
                            point.adjacentLayer = adjacent.layerIndex;
                            point.adjacentHit = true;
                            point.adjacentPoint = hit;
                            point.adjacentUV =
                                Vec2(dot(local, adjacent.plane.u),
                                     dot(local, adjacent.plane.v));
                        }
                    }
                }
                projections_.push_back(point);
            }
        }
    }
}

sj::Value Renderer::projectionPointsJson() const {
    sj::Value array = sj::Value::array();
    for (const ProjectionPoint& point : projections_) {
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(point.layerIndex);
        item["separationId"] = sj::Value(point.separationId);
        item["light"] = sj::Value(point.lightIndex);
        item["holeUV"] = sj::Value::array();
        item["holeUV"].push(sj::Value(point.holeUV.x));
        item["holeUV"].push(sj::Value(point.holeUV.y));
        item["holeWorld"] = sj::Value::array();
        item["holeWorld"].push(sj::Value(point.holeWorld.x));
        item["holeWorld"].push(sj::Value(point.holeWorld.y));
        item["holeWorld"].push(sj::Value(point.holeWorld.z));
        item["boardUV"] = sj::Value::array();
        item["boardUV"].push(sj::Value(point.boardUV.x));
        item["boardUV"].push(sj::Value(point.boardUV.y));
        item["boardPoint"] = sj::Value::array();
        item["boardPoint"].push(sj::Value(point.boardPoint.x));
        item["boardPoint"].push(sj::Value(point.boardPoint.y));
        item["boardPoint"].push(sj::Value(point.boardPoint.z));
        item["insideBackboard"] = sj::Value(point.insideBackboard);
        if (point.adjacentHit) {
            item["adjacentLayer"] = sj::Value(point.adjacentLayer);
            item["adjacentPoint"] = sj::Value::array();
            item["adjacentPoint"].push(sj::Value(point.adjacentPoint.x));
            item["adjacentPoint"].push(sj::Value(point.adjacentPoint.y));
            item["adjacentPoint"].push(sj::Value(point.adjacentPoint.z));
            item["adjacentUV"] = sj::Value::array();
            item["adjacentUV"].push(sj::Value(point.adjacentUV.x));
            item["adjacentUV"].push(sj::Value(point.adjacentUV.y));
        } else {
            item["adjacentPoint"] = sj::Value();
        }
        array.push(std::move(item));
    }
    return array;
}

sj::Value Renderer::metricsJson(const std::vector<LayerMetric>& metrics) const {
    sj::Value array = sj::Value::array();
    for (const LayerMetric& metric : metrics) {
        const double total = metric.transmitted + metric.reflected;
        const double transmissionRatio =
            total > 1.0e-12 ? metric.transmitted / total : 0.0;
        const double reflectionRatio =
            total > 1.0e-12 ? metric.reflected / total : 0.0;
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(metric.layerIndex);
        item["separationId"] = sj::Value(metric.separationId);
        item["transmittedEnergy"] = sj::Value(metric.transmitted);
        item["reflectedEnergy"] = sj::Value(metric.reflected);
        item["transmissionRatio"] = sj::Value(transmissionRatio);
        item["reflectionRatio"] = sj::Value(reflectionRatio);
        item["meanCoverage"] = sj::Value(
            metric.samples > 0.0 ? metric.coverage / metric.samples : 0.0);
        item["samples"] = sj::Value(metric.samples);
        array.push(std::move(item));
    }
    return array;
}

sj::Value Renderer::baseReport() const {
    sj::Value report = sj::Value::object();
    report["ok"] = sj::Value(true);
    report["businessCode"] = sj::Value("E_SCENE_VALID");
    report["renderer"] = sj::Value("silklight-cpu-v1");
    report["sceneDigest"] = sj::Value(scene_.sceneDigest);
    std::string summaryError;
    report["sceneSummary"] = sj::Value::parse(scene_.sceneSummaryJson, summaryError);
    if (!summaryError.empty()) {
        throw std::runtime_error("invalid internal scene summary: " + summaryError);
    }
    report["image"] = sj::Value::object();
    report["image"]["width"] = sj::Value(scene_.image.width);
    report["image"]["height"] = sj::Value(scene_.image.height);
    report["image"]["blockWidth"] = sj::Value(scene_.image.blockWidth);
    report["image"]["blockHeight"] = sj::Value(scene_.image.blockHeight);
    report["image"]["blockCount"] = sj::Value(blockCount());
    report["image"]["alphaMode"] = sj::Value("premultiplied-rgba8-srgb");
    report["image"]["toneMapping"] = sj::Value("reinhard-per-channel");
    report["camera"] = sj::Value::object();
    report["camera"]["position"] = sj::Value::array();
    report["camera"]["position"].push(sj::Value(camera_.position.x));
    report["camera"]["position"].push(sj::Value(camera_.position.y));
    report["camera"]["position"].push(sj::Value(camera_.position.z));
    report["camera"]["target"] = sj::Value::array();
    report["camera"]["target"].push(sj::Value(scene_.camera.target.x));
    report["camera"]["target"].push(sj::Value(scene_.camera.target.y));
    report["camera"]["target"].push(sj::Value(scene_.camera.target.z));
    report["camera"]["fovYDegrees"] = sj::Value(scene_.camera.fovYDegrees);

    report["halftone"] = sj::Value::array();
    for (const ScreenLayer& layer : scene_.layers) {
        sj::Value item = sj::Value::object();
        item["layer"] = sj::Value(layer.layerIndex);
        item["separationId"] = sj::Value(layer.separationId);
        item["lineCount"] = sj::Value(layer.meshLineCount);
        item["angleDegrees"] = sj::Value(layer.meshAngleDegrees);
        item["seed"] = sj::Value(static_cast<double>(layer.seed));
        item["phaseU"] = sj::Value(layer.halftone.phaseU());
        item["phaseV"] = sj::Value(layer.halftone.phaseV());
        item["tableDigest"] = sj::Value(hex64(layer.halftone.digest()));
        item["dotCoverage"] = sj::Value(layer.halftone.meanCoverage());
        report["halftone"].push(std::move(item));
    }

    report["moire"] = sj::Value::object();
    report["moire"]["thresholdDegrees"] = sj::Value(scene_.moire.thresholdDegrees);
    report["moire"]["maskStrength"] = sj::Value(scene_.moire.maskStrength);
    report["moire"]["pairs"] = sj::Value::array();
    for (const MoirePairReport& pair : moirePairs_) {
        sj::Value item = sj::Value::object();
        item["layerA"] = sj::Value(pair.layerA);
        item["layerB"] = sj::Value(pair.layerB);
        item["separationA"] = sj::Value(pair.separationA);
        item["separationB"] = sj::Value(pair.separationB);
        item["angleA"] = sj::Value(pair.angleA);
        item["angleB"] = sj::Value(pair.angleB);
        item["angleDifferenceDegrees"] = sj::Value(pair.angleDifference);
        item["thresholdDegrees"] = sj::Value(pair.threshold);
        item["maskWeight"] = sj::Value(pair.weight);
        item["triggered"] = sj::Value(pair.triggered);
        item["amplitudeBefore"] = sj::Value(pair.amplitudeBefore);
        item["amplitudeAfter"] = sj::Value(pair.amplitudeAfter);
        item["amplitudeReductionPercent"] = sj::Value(pair.reductionPercent);
        item["amplitudeBeforeByLight"] = sj::Value::array();
        item["amplitudeAfterByLight"] = sj::Value::array();
        for (int light = 0; light < 2; ++light) {
            item["amplitudeBeforeByLight"].push(
                sj::Value(pair.amplitudeBeforeByLight[static_cast<std::size_t>(light)]));
            item["amplitudeAfterByLight"].push(
                sj::Value(pair.amplitudeAfterByLight[static_cast<std::size_t>(light)]));
        }
        item["dotCoverageA"] = sj::Value(pair.dotCoverageA);
        item["dotCoverageB"] = sj::Value(pair.dotCoverageB);
        item["directDotVisibilityPreserved"] = sj::Value(true);
        report["moire"]["pairs"].push(std::move(item));
    }

    report["projection"] = sj::Value::object();
    report["projection"]["method"] =
        sj::Value("light-through-halftone-hole-perspective");
    report["projection"]["outsideBackboardPolicy"] = sj::Value("unoccluded");
    report["projection"]["points"] = projectionPointsJson();
    report["layerContributions"] = sj::Value::array();
    return report;
}

void writeRaw(const std::string& path, const std::vector<std::uint8_t>& bytes) {
    std::filesystem::path target(path);
    if (!target.parent_path().empty()) {
        std::filesystem::create_directories(target.parent_path());
    }
    std::ofstream output(target, std::ios::binary | std::ios::trunc);
    if (!output) {
        throw std::runtime_error("cannot open raw output: " + path);
    }
    output.write(reinterpret_cast<const char*>(bytes.data()),
                 static_cast<std::streamsize>(bytes.size()));
    if (!output) {
        throw std::runtime_error("failed writing raw output: " + path);
    }
}

} // namespace sl

