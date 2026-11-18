#include "renderer.hpp"

#include "sha256.hpp"

#include <algorithm>
#include <cmath>
#include <limits>
#include <set>
#include <sstream>

namespace sr {
namespace {

constexpr double kEpsilon = 1e-9;
constexpr double kMinCosIncident = 0.18;
constexpr double kEdgeFeatherWorld = 0.010;

double edgeFeather(double halfSize, double coordinate) {
    const double feather = std::min(kEdgeFeatherWorld, halfSize * 0.35);
    if (feather <= 0.0) {
        return 0.0;
    }
    return smoothstep(0.0, feather, halfSize - std::abs(coordinate));
}

uint8_t quantize(double value) {
    return static_cast<uint8_t>(
        static_cast<int>(std::floor(clampValue(value, 0.0, 1.0) * 255.0 + 0.5)));
}

}  // namespace

Renderer::Renderer(const Scene& scene, int frame) : scene_(scene), frame_(frame) {
    camera_ = scene_.cameras[static_cast<size_t>(frame_)];
    const Vec3 forward = normalize(camera_.target - camera_.position);
    const Vec3 right = normalize(cross(forward, camera_.up));
    cameraBasis_.origin = camera_.position;
    cameraBasis_.forward = forward;
    cameraBasis_.right = right;
    cameraBasis_.up = normalize(cross(right, forward));
    cameraBasis_.tanHalfFov =
        std::tan(camera_.fovDegrees * kPi / 360.0);
    cameraBasis_.aspect =
        static_cast<double>(scene_.image.width) / static_cast<double>(scene_.image.height);

    tileCols_ = (scene_.image.width + scene_.image.tileWidth - 1) / scene_.image.tileWidth;
    tileRows_ = (scene_.image.height + scene_.image.tileHeight - 1) / scene_.image.tileHeight;
    footprintsByLight_.resize(scene_.lights.size());

    for (const auto& light : scene_.lights) {
        for (const auto& particle : scene_.particles) {
            buildFootprint(particle, light);
        }
    }
    for (size_t lightIndex = 0; lightIndex < scene_.lights.size(); ++lightIndex) {
        const uint32_t lightId = scene_.lights[lightIndex].id;
        for (const auto& footprint : footprints_) {
            if (footprint.lightId == lightId) {
                footprintsByLight_[lightIndex].push_back(&footprint);
            }
        }
        std::sort(footprintsByLight_[lightIndex].begin(),
                  footprintsByLight_[lightIndex].end(),
                  [](const ShadowFootprint* a, const ShadowFootprint* b) {
                      if (a->layer != b->layer) {
                          return a->layer < b->layer;
                      }
                      return a->particleId < b->particleId;
                  });
    }
    buildSoftLut();

    std::ostringstream digestText;
    digestText << "softracer-render-v2|scene=";
    for (const uint8_t byte : scene_.digest) {
        digestText << static_cast<int>(byte) << ',';
    }
    digestText << "|frame=" << frame_ << "|t=" << formatDouble(camera_.timestamp, 15)
               << "|pixels=" << scene_.image.pixelSamples
               << "|shadow=" << scene_.render.shadowSamples
               << "|softness=" << formatDouble(scene_.render.softnessScale, 15)
               << "|exposure=" << formatDouble(scene_.render.exposure, 15);
    renderDigest_ = hexDigest(Sha256::hash(digestText.str()));
}

TileRect Renderer::tileRect(int tileIndex) const {
    TileRect rect;
    rect.index = tileIndex;
    const int tx = tileIndex % tileCols_;
    const int ty = tileIndex / tileCols_;
    rect.x = tx * scene_.image.tileWidth;
    rect.y = ty * scene_.image.tileHeight;
    rect.width = std::min(scene_.image.tileWidth, scene_.image.width - rect.x);
    rect.height = std::min(scene_.image.tileHeight, scene_.image.height - rect.y);
    return rect;
}

Vec3 Renderer::rayDirection(double pixelX, double pixelY, double subX, double subY) const {
    const double nx =
        ((pixelX + subX) / static_cast<double>(scene_.image.width)) * 2.0 - 1.0;
    const double ny =
        1.0 - ((pixelY + subY) / static_cast<double>(scene_.image.height)) * 2.0;
    return normalize(cameraBasis_.forward +
                     cameraBasis_.right * (nx * cameraBasis_.aspect *
                                          cameraBasis_.tanHalfFov) +
                     cameraBasis_.up * (ny * cameraBasis_.tanHalfFov));
}

Vec3 Renderer::backgroundForRay(Vec3 direction, bool& hitGround) const {
    hitGround = false;
    const double denom = dot(scene_.ground.normal, direction);
    if (std::abs(denom) > 1e-10) {
        const double t = dot(scene_.ground.normal, scene_.ground.center -
                                                        cameraBasis_.origin) /
                         denom;
        if (t > 1e-7) {
            const Vec3 point = cameraBasis_.origin + direction * t;
            bool onPlane = false;
            const Vec2 uv = groundCoordinates(scene_.ground, point, &onPlane);
            if (onPlane && insideGround(scene_.ground, uv)) {
                hitGround = true;
                return shadeGround(point).linear;
            }
        }
    }
    const double up = saturate(direction.y * 0.5 + 0.5);
    return Vec3{0.006 + 0.016 * up, 0.009 + 0.024 * up, 0.018 + 0.042 * up};
}

Renderer::GroundShade Renderer::shadeGround(Vec3 point) const {
    GroundShade shade;
    shade.ambientLinear = scene_.ground.albedo * scene_.render.ambient;
    shade.linear = shade.ambientLinear;

    for (size_t lightIndex = 0; lightIndex < scene_.lights.size(); ++lightIndex) {
        const PointLight& light = scene_.lights[lightIndex];
        const Vec3 toLight = light.position - point;
        const double distance2 = length2(toLight);
        if (distance2 < 1e-12) {
            continue;
        }
        const Vec3 direction = toLight / std::sqrt(distance2);
        const double ndotl = std::max(0.0, dot(scene_.ground.normal, direction));
        if (ndotl <= 0.0) {
            continue;
        }
        const double visibility =
            shadowVisibility(point, light, footprintsByLight_[lightIndex]);
        const double irradiance =
            light.intensity * ndotl / (1.0 + distance2);
        const Vec3 radiance = light.color * (irradiance * visibility);
        const Vec3 groundContribution = scene_.ground.albedo * radiance;
        shade.lightLinear += groundContribution;
        shade.linear += groundContribution;

        LightProbeContribution contribution;
        contribution.lightId = light.id;
        contribution.linearRadiance = luminance(groundContribution);
        contribution.visibility = visibility;
        shade.contributions.push_back(contribution);
    }
    return shade;
}

void Renderer::buildFootprint(const Particle& particle, const PointLight& light) {
    ShadowFootprint footprint;
    footprint.particleId = particle.id;
    footprint.layer = particle.layer;
    footprint.lightId = light.id;
    footprint.groundDistance = groundDistanceForParticle(particle);
    footprint.lightDistance = lightDistanceForParticle(particle, light);
    footprint.transitionWidth =
        scene_.render.softnessScale *
        (footprint.groundDistance + footprint.lightDistance);
    if (footprint.transitionWidth < scene_.render.softnessScale) {
        footprint.transitionWidth = scene_.render.softnessScale;
    }
    const double particleDiagonal =
        std::hypot(particle.width, particle.height);
    if (footprint.lightDistance <
        1.2 * std::max(particle.width, particle.height)) {
        footprints_.push_back(std::move(footprint));
        return;
    }

    Vec3 centerProjected;
    if (!projectPointToGround(scene_.ground, light, particle.center, centerProjected)) {
        footprints_.push_back(std::move(footprint));
        return;
    }
    footprint.projectedCenterWorld = centerProjected;
    footprint.projectedCenterUv = groundCoordinates(scene_.ground, centerProjected);
    footprint.insideGround = insideGround(scene_.ground, footprint.projectedCenterUv);

    std::vector<Vec2> corners;
    corners.reserve(4);
    double maximumCornerRadius = 0.0;
    for (const auto& corner : particle.corners) {
        Vec3 projected;
        if (!projectPointToGround(scene_.ground, light, corner, projected)) {
            footprint.insideGround = false;
            footprints_.push_back(std::move(footprint));
            return;
        }
        const Vec2 uv = groundCoordinates(scene_.ground, projected);
        corners.push_back(uv);
        maximumCornerRadius =
            std::max(maximumCornerRadius,
                     length(uv - footprint.projectedCenterUv));
    }
    const double expectedRadius =
        particleDiagonal *
        (1.0 + footprint.groundDistance /
                   std::max(footprint.lightDistance, 1e-6));
    if (maximumCornerRadius > 8.0 * expectedRadius) {
        footprint.insideGround = false;
        footprints_.push_back(std::move(footprint));
        return;
    }
    footprint.hull = convexHull(corners);
    if (footprint.hull.size() < 3U) {
        footprint.insideGround = false;
        footprints_.push_back(std::move(footprint));
        return;
    }
    footprint.validProjection = true;
    const Vec3 toLight = light.position - particle.center;
    const Vec3 lightDir = normalize(toLight);
    const double cosIncident = std::max(kMinCosIncident,
                                        std::abs(dot(particle.normal, lightDir)));
    const double effectiveTransmittance =
        std::pow(clampValue(particle.transmittance, 0.0, 1.0),
                 1.0 / cosIncident);
    footprint.alpha = saturate(1.0 - effectiveTransmittance);
    footprint.active = footprint.insideGround && footprint.alpha > 1e-8;
    footprints_.push_back(std::move(footprint));
}

bool Renderer::projectPointToGround(const Ground& ground, const PointLight& light,
                                    Vec3 point, Vec3& projected) {
    const Vec3 ray = point - light.position;
    const double denom = dot(ground.normal, ray);
    if (std::abs(denom) < 1e-10) {
        return false;
    }
    const double t =
        dot(ground.normal, ground.center - light.position) / denom;
    if (!(t > 1e-7) || !isFinite(t) || t > 1e8) {
        return false;
    }
    projected = light.position + ray * t;
    return isFinite(projected.x) && isFinite(projected.y) && isFinite(projected.z);
}

std::vector<Vec2> Renderer::convexHull(std::vector<Vec2> points) {
    std::sort(points.begin(), points.end(), [](Vec2 a, Vec2 b) {
        if (a.x != b.x) {
            return a.x < b.x;
        }
        return a.y < b.y;
    });
    points.erase(std::unique(points.begin(), points.end(), [](Vec2 a, Vec2 b) {
                     return std::abs(a.x - b.x) < 1e-11 &&
                            std::abs(a.y - b.y) < 1e-11;
                 }),
                 points.end());
    if (points.size() < 3U) {
        return {};
    }
    auto crossZ = [](Vec2 o, Vec2 a, Vec2 b) {
        return cross(a - o, b - o);
    };
    std::vector<Vec2> lower;
    for (const Vec2 p : points) {
        while (lower.size() >= 2U &&
               crossZ(lower[lower.size() - 2U], lower.back(), p) <= 1e-12) {
            lower.pop_back();
        }
        lower.push_back(p);
    }
    std::vector<Vec2> upper;
    for (auto it = points.rbegin(); it != points.rend(); ++it) {
        while (upper.size() >= 2U &&
               crossZ(upper[upper.size() - 2U], upper.back(), *it) <= 1e-12) {
            upper.pop_back();
        }
        upper.push_back(*it);
    }
    lower.pop_back();
    upper.pop_back();
    lower.insert(lower.end(), upper.begin(), upper.end());
    if (lower.size() < 3U) {
        return {};
    }
    double area = 0.0;
    for (size_t i = 0; i < lower.size(); ++i) {
        area += cross(lower[i], lower[(i + 1U) % lower.size()]);
    }
    if (std::abs(area) < 1e-12) {
        return {};
    }
    if (area < 0.0) {
        std::reverse(lower.begin(), lower.end());
    }
    return lower;
}

double Renderer::distanceToSegment(Vec2 p, Vec2 a, Vec2 b) {
    const Vec2 ab = b - a;
    const double denom = length2(ab);
    if (denom < 1e-20) {
        return length(p - a);
    }
    const double t = clampValue(dot(p - a, ab) / denom, 0.0, 1.0);
    return length(p - (a + ab * t));
}

double Renderer::signedDistanceToHull(const std::vector<Vec2>& hull, Vec2 p) {
    bool inside = true;
    double minDistance = std::numeric_limits<double>::infinity();
    for (size_t i = 0; i < hull.size(); ++i) {
        const Vec2 a = hull[i];
        const Vec2 b = hull[(i + 1U) % hull.size()];
        if (cross(b - a, p - a) < -1e-12) {
            inside = false;
        }
        minDistance = std::min(minDistance, distanceToSegment(p, a, b));
    }
    return inside ? -minDistance : minDistance;
}

void Renderer::buildSoftLut() {
    const int samples = scene_.render.shadowSamples;
    softLut_.resize(static_cast<size_t>(samples));
    for (int i = 0; i < samples; ++i) {
        const double t = static_cast<double>(i) / static_cast<double>(samples - 1);
        softLut_[static_cast<size_t>(i)] = smoothstep(0.0, 1.0, t);
    }
}

double Renderer::sampleSoftCoverage(double normalized) const {
    const double t = clampValue(normalized, 0.0, 1.0);
    const double position = t * static_cast<double>(softLut_.size() - 1U);
    const size_t index = static_cast<size_t>(std::floor(position));
    const size_t next = std::min(index + 1U, softLut_.size() - 1U);
    const double fraction = position - static_cast<double>(index);
    const double ramp =
        softLut_[index] * (1.0 - fraction) + softLut_[next] * fraction;
    return 1.0 - ramp;
}

double Renderer::footprintCoverage(const ShadowFootprint& footprint, Vec2 uv) const {
    if (!footprint.active) {
        return 0.0;
    }
    const double signedDistance = signedDistanceToHull(footprint.hull, uv);
    const double normalized =
        0.5 + signedDistance / std::max(footprint.transitionWidth, 1e-8);
    return sampleSoftCoverage(normalized);
}

double Renderer::shadowVisibility(
    Vec3 groundPointValue, const PointLight& light,
    const std::vector<const ShadowFootprint*>& lightFootprints) const {
    const Vec2 uv = groundCoordinates(scene_.ground, groundPointValue);
    double layerTransmission = 1.0;
    double layerTau = 0.0;
    int currentLayer = -1;
    for (const ShadowFootprint* footprint : lightFootprints) {
        if (!footprint->active) {
            continue;
        }
        if (currentLayer < 0) {
            currentLayer = footprint->layer;
        } else if (footprint->layer != currentLayer) {
            layerTransmission *= std::exp(-layerTau);
            layerTau = 0.0;
            currentLayer = footprint->layer;
        }
        const double coverage = footprintCoverage(*footprint, uv);
        const double absorbed =
            clampValue(footprint->alpha * coverage, 0.0, 0.999999999);
        if (absorbed > 1e-12) {
            layerTau += -std::log1p(-absorbed);
        }
    }
    if (currentLayer >= 0) {
        layerTransmission *= std::exp(-layerTau);
    }
    (void)light;
    return clampValue(layerTransmission, 0.0, 1.0);
}

bool Renderer::intersectParticle(const Particle& particle, Vec3 origin, Vec3 direction,
                                 double& t, double& localX, double& localY,
                                 double& cosIncident) {
    const double denom = dot(particle.normal, direction);
    if (std::abs(denom) < 1e-10) {
        return false;
    }
    t = dot(particle.normal, particle.center - origin) / denom;
    if (!(t > 1e-6) || !isFinite(t)) {
        return false;
    }
    const Vec3 hit = origin + direction * t;
    const Vec3 delta = hit - particle.center;
    localX = dot(delta, particle.right);
    localY = dot(delta, particle.up);
    if (std::abs(localX) > particle.width * 0.5 ||
        std::abs(localY) > particle.height * 0.5) {
        return false;
    }
    cosIncident = std::abs(denom);
    return true;
}

void Renderer::composeParticle(const Particle& particle, double cosIncident, Vec3& smoke,
                               double& alpha, std::vector<OwnerScore>& scores,
                               bool collectOwner) const {
    const double effectiveTransmittance =
        std::pow(clampValue(particle.transmittance, 0.0, 1.0),
                 1.0 / std::max(cosIncident, kMinCosIncident));
    const double baseAlpha = saturate(1.0 - effectiveTransmittance);
    if (baseAlpha <= 1e-10) {
        return;
    }
    const double halfWidth = particle.width * 0.5;
    const double halfHeight = particle.height * 0.5;
    const double edge =
        edgeFeather(halfWidth, 0.0) * edgeFeather(halfHeight, 0.0);
    const double sourceAlpha = saturate(baseAlpha * edge);
    if (sourceAlpha <= 1e-10) {
        return;
    }
    const double oldAlpha = alpha;
    const Vec3 source = particle.color * sourceAlpha;
    smoke = source + smoke * (1.0 - sourceAlpha);
    alpha = sourceAlpha + oldAlpha * (1.0 - sourceAlpha);
    if (collectOwner) {
        const double weight = sourceAlpha * (1.0 - oldAlpha);
        addOwnerScore(scores, particle.id, weight);
    }
}

Renderer::RayResult Renderer::traceRay(Vec3 origin, Vec3 direction, bool collectOwner) const {
    RayResult result;
    bool hitGround = false;
    const Vec3 background = backgroundForRay(direction, hitGround);
    result.hitGround = hitGround;

    double alpha = 0.0;
    Vec3 smoke;
    if (collectOwner) {
        result.ownerScores.reserve(8);
    }
    for (const auto& particle : scene_.particles) {
        double t = 0.0;
        double localX = 0.0;
        double localY = 0.0;
        double cosIncident = 0.0;
        if (!intersectParticle(particle, origin, direction, t, localX, localY,
                               cosIncident)) {
            continue;
        }
        const double halfWidth = particle.width * 0.5;
        const double halfHeight = particle.height * 0.5;
        const double edge = edgeFeather(halfWidth, localX) *
                            edgeFeather(halfHeight, localY);
        if (edge <= 1e-10) {
            continue;
        }
        const double effectiveTransmittance =
            std::pow(clampValue(particle.transmittance, 0.0, 1.0),
                     1.0 / std::max(cosIncident, kMinCosIncident));
        const double sourceAlpha =
            saturate((1.0 - effectiveTransmittance) * edge);
        if (sourceAlpha <= 1e-10) {
            continue;
        }
        ++result.visibleFragments;
        result.transmittanceSum += effectiveTransmittance;
        const double oldAlpha = alpha;
        smoke = particle.color * sourceAlpha + smoke * (1.0 - sourceAlpha);
        alpha = sourceAlpha + oldAlpha * (1.0 - sourceAlpha);
        if (collectOwner) {
            addOwnerScore(result.ownerScores, particle.id,
                          sourceAlpha * (1.0 - oldAlpha));
        }
    }
    result.smokePremul = smoke;
    result.smokeAlpha = alpha;
    result.finalLinear = smoke + background * (1.0 - alpha);
    return result;
}

void Renderer::addOwnerScore(std::vector<OwnerScore>& scores, uint32_t id,
                             double weight) const {
    for (auto& score : scores) {
        if (score.id == id) {
            score.weight += weight;
            return;
        }
    }
    scores.push_back(OwnerScore{id, weight});
}

uint32_t Renderer::bestOwner(const std::vector<OwnerScore>& scores) const {
    uint32_t bestId = kNoParticleId;
    double bestWeight = -1.0;
    for (const auto& score : scores) {
        if (score.weight > bestWeight + 1e-15 ||
            (std::abs(score.weight - bestWeight) <= 1e-15 &&
             (bestId == kNoParticleId || score.id < bestId))) {
            bestWeight = score.weight;
            bestId = score.id;
        }
    }
    return bestId;
}

TileResult Renderer::renderTile(int tileIndex) const {
    TileResult tile;
    tile.rect = tileRect(tileIndex);
    const size_t pixelCount =
        static_cast<size_t>(tile.rect.width) * static_cast<size_t>(tile.rect.height);
    tile.finalRgba.resize(pixelCount * 4U);
    tile.smokeRgba.resize(pixelCount * 4U);
    tile.ownership.resize(pixelCount, kNoParticleId);
    for (const auto& light : scene_.lights) {
        tile.stats.shadowSamplePoints[light.id] = 0;
    }

    const int sampleCount = scene_.image.pixelSamples;
    const double sampleStep = 1.0 / static_cast<double>(sampleCount);
    for (int localY = 0; localY < tile.rect.height; ++localY) {
        for (int localX = 0; localX < tile.rect.width; ++localX) {
            const int pixelX = tile.rect.x + localX;
            const int pixelY = tile.rect.y + localY;
            const size_t pixelIndex =
                static_cast<size_t>(localY) * static_cast<size_t>(tile.rect.width) +
                static_cast<size_t>(localX);

            Vec3 finalSum;
            Vec3 smokeSum;
            double alphaSum = 0.0;
            std::vector<OwnerScore> ownerScores;
            for (int sy = 0; sy < sampleCount; ++sy) {
                for (int sx = 0; sx < sampleCount; ++sx) {
                    const double subX =
                        (static_cast<double>(sx) + 0.5) * sampleStep;
                    const double subY =
                        (static_cast<double>(sy) + 0.5) * sampleStep;
                    const Vec3 direction =
                        rayDirection(static_cast<double>(pixelX),
                                     static_cast<double>(pixelY), subX, subY);
                    RayResult sample =
                        traceRay(cameraBasis_.origin, direction, true);
                    finalSum += sample.finalLinear;
                    smokeSum += sample.smokePremul;
                    alphaSum += sample.smokeAlpha;
                    for (const auto& score : sample.ownerScores) {
                        addOwnerScore(ownerScores, score.id, score.weight);
                    }
                }
            }
            const double inverseSamples =
                1.0 / static_cast<double>(sampleCount * sampleCount);
            const Vec3 finalLinear = finalSum * inverseSamples;
            const Vec3 smokePremul = smokeSum * inverseSamples;
            const double smokeAlpha =
                clampValue(alphaSum * inverseSamples, 0.0, 1.0);
            const Vec3 mapped = toneMap(finalLinear);

            const size_t offset = pixelIndex * 4U;
            tile.finalRgba[offset] = quantize(mapped.x);
            tile.finalRgba[offset + 1U] = quantize(mapped.y);
            tile.finalRgba[offset + 2U] = quantize(mapped.z);
            tile.finalRgba[offset + 3U] = 255U;
            tile.smokeRgba[offset] =
                quantize(std::min(smokePremul.x, smokeAlpha));
            tile.smokeRgba[offset + 1U] =
                quantize(std::min(smokePremul.y, smokeAlpha));
            tile.smokeRgba[offset + 2U] =
                quantize(std::min(smokePremul.z, smokeAlpha));
            tile.smokeRgba[offset + 3U] = quantize(smokeAlpha);
            tile.ownership[pixelIndex] = bestOwner(ownerScores);

            const Vec3 centerDirection =
                rayDirection(static_cast<double>(pixelX),
                             static_cast<double>(pixelY), 0.5, 0.5);
            RayResult center =
                traceRay(cameraBasis_.origin, centerDirection, false);
            tile.stats.visibleFragments += center.visibleFragments;
            tile.stats.transmittanceSamples += center.visibleFragments;
            tile.stats.transmittanceSum += center.transmittanceSum;
            if (center.hitGround) {
                const double denom = dot(scene_.ground.normal, centerDirection);
                if (std::abs(denom) > 1e-10) {
                    const double t =
                        dot(scene_.ground.normal,
                            scene_.ground.center - cameraBasis_.origin) /
                        denom;
                    if (t > 0.0) {
                        const Vec3 point =
                            cameraBasis_.origin + centerDirection * t;
                        const Vec2 uv =
                            groundCoordinates(scene_.ground, point);
                        if (insideGround(scene_.ground, uv)) {
                            const uint64_t densitySamples =
                                static_cast<uint64_t>(scene_.render.shadowSamples) *
                                static_cast<uint64_t>(scene_.render.shadowSamples);
                            for (const auto& light : scene_.lights) {
                                tile.stats.shadowSamplePoints[light.id] +=
                                    densitySamples;
                            }
                        }
                    }
                }
            }
        }
    }
    return tile;
}

Vec3 Renderer::applyToneMap(Vec3 linear, double exposure) {
    Vec3 mapped;
    mapped.x = 1.0 - std::exp(-std::max(0.0, linear.x) * exposure);
    mapped.y = 1.0 - std::exp(-std::max(0.0, linear.y) * exposure);
    mapped.z = 1.0 - std::exp(-std::max(0.0, linear.z) * exposure);
    mapped.x = std::pow(mapped.x, 1.0 / 2.2);
    mapped.y = std::pow(mapped.y, 1.0 / 2.2);
    mapped.z = std::pow(mapped.z, 1.0 / 2.2);
    return mapped;
}

Vec3 Renderer::toneMap(Vec3 linear) const {
    return applyToneMap(linear, scene_.render.exposure);
}

std::vector<ProbeEvaluation> Renderer::evaluateProbes() const {
    std::vector<Vec2> probeUvs;
    std::vector<std::string> probeIds;
    if (!scene_.render.probes.empty()) {
        for (const auto& probe : scene_.render.probes) {
            probeUvs.push_back(probe.groundUv);
            probeIds.push_back(probe.id);
        }
    } else {
        const double u = scene_.ground.halfU;
        const double v = scene_.ground.halfV;
        const double fractions[] = {-0.55, -0.28, 0.0, 0.24, 0.52};
        for (size_t i = 0; i < sizeof(fractions) / sizeof(fractions[0]); ++i) {
            probeUvs.push_back(Vec2{u * fractions[i], -0.22 * v});
            probeIds.push_back("auto-" + std::to_string(i));
        }
        for (const auto& light : scene_.lights) {
            const ShadowFootprint* chosen = nullptr;
            for (const auto& footprint : footprints_) {
                if (footprint.lightId != light.id || !footprint.active) {
                    continue;
                }
                if (chosen == nullptr ||
                    footprint.alpha > chosen->alpha + 1e-12 ||
                    (std::abs(footprint.alpha - chosen->alpha) <= 1e-12 &&
                     footprint.particleId < chosen->particleId)) {
                    chosen = &footprint;
                }
            }
            if (chosen != nullptr) {
                probeUvs.push_back(chosen->projectedCenterUv);
                probeIds.push_back("auto-shadow-" + std::to_string(light.id) + "-" +
                                   std::to_string(chosen->particleId));
            }
        }
    }

    std::vector<ProbeEvaluation> evaluations;
    for (size_t probeIndex = 0; probeIndex < probeUvs.size(); ++probeIndex) {
        ProbeEvaluation result;
        result.id = probeIds[probeIndex];
        result.lightId =
            scene_.render.probes.empty() ? 0U : scene_.render.probes[probeIndex].lightId;
        result.groundUv = probeUvs[probeIndex];
        result.world = groundPoint(scene_.ground, result.groundUv);
        result.insideGround = insideGround(scene_.ground, result.groundUv);
        const GroundShade shade = shadeGround(result.world);
        result.linearLuminance = luminance(shade.linear);
        result.mappedLuminance = luminance(toneMap(shade.linear));
        double total = 0.0;
        for (const auto& contribution : shade.contributions) {
            total += contribution.linearRadiance;
            result.contributions.push_back(contribution);
        }
        result.totalLightLinear = total;
        double previousShares = 0.0;
        const size_t count = result.contributions.size();
        for (size_t i = 0; i < count; ++i) {
            if (i + 1U == count) {
                result.contributions[i].share = 1.0 - previousShares;
            } else if (total > 1e-15) {
                result.contributions[i].share =
                    result.contributions[i].linearRadiance / total;
            } else {
                result.contributions[i].share = 1.0 / static_cast<double>(count);
            }
            previousShares += result.contributions[i].share;
        }
        double shareSum = 0.0;
        for (const auto& contribution : result.contributions) {
            shareSum += contribution.share;
        }
        result.shareSum = shareSum;
        result.superpositionExcess =
            result.linearLuminance - (luminance(shade.ambientLinear) + total);
        evaluations.push_back(std::move(result));
    }
    return evaluations;
}

std::vector<EdgeProfile> Renderer::edgeProfiles() const {
    std::vector<EdgeProfile> profiles;
    for (const auto& light : scene_.lights) {
        const ShadowFootprint* chosen = nullptr;
        size_t bestEdge = 0;
        double chosenArea = -1.0;
        double bestInsideScore = -1.0;
        for (const auto& footprint : footprints_) {
            if (footprint.lightId != light.id || !footprint.active) {
                continue;
            }
            double area = 0.0;
            for (size_t i = 0; i < footprint.hull.size(); ++i) {
                area += cross(footprint.hull[i],
                              footprint.hull[(i + 1U) % footprint.hull.size()]);
            }
            area = std::abs(area) * 0.5;
            if (chosen == nullptr || area > chosenArea + 1e-14) {
                chosen = &footprint;
                chosenArea = area;
                bestEdge = 0;
            }
            for (size_t edgeIndex = 0; edgeIndex < footprint.hull.size();
                 ++edgeIndex) {
                const Vec2 a = footprint.hull[edgeIndex];
                const Vec2 b =
                    footprint.hull[(edgeIndex + 1U) % footprint.hull.size()];
                const Vec2 edgeVector = b - a;
                const Vec2 outward =
                    normalize(Vec2{edgeVector.y, -edgeVector.x});
                const Vec2 midpoint = (a + b) * 0.5;
                const double width = footprint.transitionWidth;
                if (insideGround(scene_.ground, midpoint) &&
                    insideGround(scene_.ground, midpoint - outward * width) &&
                    insideGround(scene_.ground, midpoint + outward * width)) {
                    const double score =
                        area * 100.0 + std::abs(outward.x);
                    if (score > bestInsideScore) {
                        bestInsideScore = score;
                        chosen = &footprint;
                        chosenArea = area;
                        bestEdge = edgeIndex;
                    }
                }
            }
        }
        if (chosen == nullptr || chosen->hull.size() < 3U) {
            continue;
        }

        if (bestInsideScore < 0.0) {
            double bestOutwardX = -1.0;
            for (size_t i = 0; i < chosen->hull.size(); ++i) {
                const Vec2 a = chosen->hull[i];
                const Vec2 b = chosen->hull[(i + 1U) % chosen->hull.size()];
                const Vec2 edge = b - a;
                const Vec2 outward = normalize(Vec2{edge.y, -edge.x});
                const double outwardX = std::abs(outward.x);
                if (outwardX > bestOutwardX) {
                    bestOutwardX = outwardX;
                    bestEdge = i;
                }
            }
        }
        const Vec2 a = chosen->hull[bestEdge];
        const Vec2 b = chosen->hull[(bestEdge + 1U) % chosen->hull.size()];
        const Vec2 midpoint = (a + b) * 0.5;
        const Vec2 edge = b - a;
        const Vec2 outward = normalize(Vec2{edge.y, -edge.x});

        EdgeProfile profile;
        profile.lightId = light.id;
        profile.particleId = chosen->particleId;
        profile.layer = chosen->layer;
        profile.transitionWidth = chosen->transitionWidth;
        const int offsets = 9;
        for (int i = 0; i < offsets; ++i) {
            const double t = -1.0 + 2.0 * static_cast<double>(i) /
                                       static_cast<double>(offsets - 1);
            const double offset = t * chosen->transitionWidth;
            const Vec2 uv = midpoint + outward * offset;
            const Vec3 world = groundPoint(scene_.ground, uv);
            EdgeProfileSample sample;
            sample.offset = offset;
            sample.normalizedOffset = t;
            sample.coverage = footprintCoverage(*chosen, uv);
            if (insideGround(scene_.ground, uv)) {
                sample.linearLuminance =
                    luminance(shadeGround(world).linear);
            }
            profile.samples.push_back(sample);
        }
        profiles.push_back(std::move(profile));
    }
    return profiles;
}

double Renderer::groundDistanceForParticle(const Particle& particle) const {
    return std::abs(dot(particle.center - scene_.ground.center,
                        scene_.ground.normal));
}

double Renderer::lightDistanceForParticle(const Particle& particle,
                                          const PointLight& light) const {
    return length(particle.center - light.position);
}

}  // namespace sr
