#include "renderer.h"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <limits>
#include <map>
#include <vector>

#include "math3d.h"

namespace caisson {
namespace {

struct CameraVertex {
    Vec3 camera;
    Vec3 world;
    Vec2 uv;
};

struct ScreenVertex {
    double x = 0.0;
    double y = 0.0;
    double depth = 0.0;
    Vec3 world;
    Vec2 uv;
};

struct LayerHit {
    int layerIndex = -1;
    int quadrant = -1;
    int triangleIndex = 0;
    double depth = 0.0;
    Vec3 world;
    Vec2 uv;
    Vec3 normal;
};

struct OccluderHit {
    bool valid = false;
    int occluderIndex = -1;
    int triangleIndex = 0;
    double depth = std::numeric_limits<double>::infinity();
    Vec3 world;
    Vec3 normal;
};

struct FragmentShade {
    double alpha = 0.0;
    Vec3 premultiplied{};
    Vec3 transmit{};
    Vec3 reflect{};
    Vec3 diffuse{};
    double woodWeight = 0.0;
    double gauzeWeight = 0.0;
};

struct SampleResult {
    Vec3 premultiplied{};
    double alpha = 0.0;
    bool covered = false;
    int nearestLayer = 0;
    int visibleLayers = 0;
    std::array<double, 256> layerContribution{};
    double occluderContribution = 0.0;
};

struct PixelAccumulator {
    Vec3 premulSum{};
    double alphaSum = 0.0;
    std::array<int, 256> nearestVotes{};
    std::array<double, 256> layerWeights{};
    double occluderWeight = 0.0;
    int coveredCount = 0;
    int visibleLayerCountSum = 0;
};

CameraVertex toCamera(const Vec3& world, const Vec2& uv, const Camera& camera) {
    const Vec3 relative = world - camera.position;
    CameraVertex output;
    output.camera = {
        dot(relative, camera.right),
        dot(relative, camera.correctedUp),
        dot(relative, camera.forward),
    };
    output.world = world;
    output.uv = uv;
    return output;
}

CameraVertex interpolate(const CameraVertex& a, const CameraVertex& b, double t) {
    CameraVertex output;
    output.camera = lerp(a.camera, b.camera, t);
    output.world = lerp(a.world, b.world, t);
    output.uv = lerp(a.uv, b.uv, t);
    return output;
}

std::vector<CameraVertex> clipNear(
    const std::array<CameraVertex, 3>& triangle,
    double nearZ) {
    std::vector<CameraVertex> output;
    for (int i = 0; i < 3; ++i) {
        const CameraVertex& current = triangle[static_cast<std::size_t>(i)];
        const CameraVertex& next = triangle[static_cast<std::size_t>((i + 1) % 3)];
        const bool currentInside = current.camera.z >= nearZ;
        const bool nextInside = next.camera.z >= nearZ;
        if (currentInside) output.push_back(current);
        if (currentInside != nextInside) {
            const double denominator = next.camera.z - current.camera.z;
            if (std::abs(denominator) > 1.0e-15) {
                const double t = (nearZ - current.camera.z) / denominator;
                output.push_back(interpolate(current, next, t));
            }
        }
    }
    return output;
}

ScreenVertex project(
    const CameraVertex& vertex,
    const Camera& camera,
    int sampleWidth,
    int sampleHeight) {
    const double aspect =
        static_cast<double>(sampleWidth) / static_cast<double>(sampleHeight);
    const double focal =
        1.0 / std::tan(camera.verticalFovDegrees * kPi / 360.0);
    const double ndcX = vertex.camera.x / vertex.camera.z * focal / aspect;
    const double ndcY = vertex.camera.y / vertex.camera.z * focal;
    ScreenVertex output;
    output.x = (ndcX + 1.0) * 0.5 * static_cast<double>(sampleWidth);
    output.y = (1.0 - (ndcY + 1.0) * 0.5) * static_cast<double>(sampleHeight);
    output.depth = vertex.camera.z;
    output.world = vertex.world;
    output.uv = vertex.uv;
    return output;
}

double edgeFunction(const ScreenVertex& a, const ScreenVertex& b, double x, double y) {
    return (b.x - a.x) * (y - a.y) - (b.y - a.y) * (x - a.x);
}

Vec3 triangleNormal(const Vec3& a, const Vec3& b, const Vec3& c) {
    return normalize(cross(b - a, c - a));
}

double latticeMask(
    const Vec2& uv,
    int columns,
    int rows,
    double latticeWidth) {
    const double u = uv.x - std::floor(uv.x);
    const double v = uv.y - std::floor(uv.y);
    const double fu = u * static_cast<double>(columns);
    const double fv = v * static_cast<double>(rows);
    const double du = std::min(fu - std::floor(fu), std::ceil(fu) - fu);
    const double dv = std::min(fv - std::floor(fv), std::ceil(fv) - fv);
    const double distance = std::min(du, dv);
    const double soft = std::max(0.006, latticeWidth * 0.18);
    const double inner = latticeWidth * 0.50;
    const double outer = inner + soft;
    if (distance <= inner) return 1.0;
    if (distance >= outer) return 0.0;
    const double t = (outer - distance) / (outer - inner);
    return t * t * (3.0 - 2.0 * t);
}

Vec3 toneMap(const Vec3& input) {
    const double luma = std::max(0.0, luminance(input));
    if (luma <= 1.0e-14) return {0.0, 0.0, 0.0};
    const double mappedLuma = 0.955 * (1.0 - std::exp(-1.30 * luma));
    const double scale = mappedLuma / luma;
    const Vec3 mapped = input * scale;
    return {
        std::clamp(mapped.x, 0.0, 0.98),
        std::clamp(mapped.y, 0.0, 0.98),
        std::clamp(mapped.z, 0.0, 0.98),
    };
}

double fixedEnergy(double value) {
    if (!(value > 0.0)) return 0.0;
    return std::round(value * 1000000.0);
}

FragmentShade shadeLayer(
    const Scene& scene,
    const Layer& layer,
    const LayerTexture& texture,
    const Vec3& world,
    const Vec2& uv,
    const Vec3& normal,
    const ShadowSampler& shadowSampler) {
    FragmentShade output;
    const Vec3 view = normalize(scene.camera.position - world);
    const double ndv = std::clamp(std::abs(dot(normal, view)), 0.0, 1.0);
    const double wood = latticeMask(
        uv, texture.columnsHint, texture.rowsHint, layer.latticeWidth);
    const double gauze = 1.0 - wood;
    output.woodWeight = wood;
    output.gauzeWeight = gauze;

    const Vec3 textureColor = sampleWood(texture, uv);
    const Vec3 albedo = clamp01({
        layer.woodColor.x * textureColor.x,
        layer.woodColor.y * textureColor.y,
        layer.woodColor.z * textureColor.z,
    });
    const Vec3 silkTint = clamp01(lerp(layer.woodColor, Vec3{1.0, 0.93, 0.82}, 0.42));
    const double transmittance = std::clamp(
        layer.transmittance * (0.86 + 0.14 * sampleGauze(texture, uv)),
        0.0,
        1.0);
    const double fresnel = std::clamp(
        layer.reflectance + (1.0 - layer.reflectance) * std::pow(1.0 - ndv, 5.0),
        0.0,
        1.0);

    Vec3 diffuse{};
    Vec3 transmit{};
    Vec3 reflect{};

    for (const Lamp& lamp : scene.lamps) {
        const Vec3 toLamp = lamp.position - world;
        const double distance = std::max(length(toLamp), 1.0e-6);
        const Vec3 lightDirection = toLamp / distance;
        const double ndl = std::clamp(std::abs(dot(normal, lightDirection)), 0.0, 1.0);
        const double attenuation = 1.0 / (1.0 + 0.42 * distance * distance);
        const double visibility = shadowSampler.visibility(world, lamp);
        const Vec3 irradiance =
            lamp.color * (lamp.intensity * attenuation * visibility);

        diffuse += albedo * irradiance * (0.15 + 0.85 * ndl);
        transmit += silkTint * irradiance * transmittance * (0.66 + 0.34 * ndl);

        const Vec3 halfVector = normalize(lightDirection + view);
        const double specular = std::pow(
            std::clamp(std::abs(dot(normal, halfVector)), 0.0, 1.0),
            44.0);
        reflect += lamp.color * (lamp.intensity * attenuation * visibility) *
                   fresnel * specular * 1.35;
    }

    diffuse += albedo * scene.ambient * (0.48 + 0.52 * ndv);
    transmit += silkTint * scene.ambient * transmittance * (0.52 + 0.48 * ndv);
    reflect += Vec3{0.018, 0.014, 0.010} * fresnel * (0.55 + 0.45 * ndv);

    output.diffuse = diffuse * wood;
    output.transmit = transmit * gauze;
    output.reflect = reflect * gauze;
    const Vec3 radiance = toneMap(output.diffuse + output.transmit + output.reflect);
    const double alpha = std::clamp(
        wood * (0.90 + 0.04 * ndv) +
            gauze * (0.24 + 0.34 * layer.reflectance + 0.14 * (1.0 - transmittance)),
        0.04,
        0.98);
    output.alpha = alpha;
    output.premultiplied = radiance * alpha;
    return output;
}

Vec3 shadeOccluder(
    const Scene& scene,
    const Occluder& occluder,
    const Vec3& world,
    const Vec3& normal) {
    const Vec3 view = normalize(scene.camera.position - world);
    const double ndv = std::clamp(std::abs(dot(normal, view)), 0.0, 1.0);
    Vec3 color = occluder.color * scene.ambient * (0.55 + 0.45 * ndv);
    for (const Lamp& lamp : scene.lamps) {
        const Vec3 toLamp = lamp.position - world;
        const double distance = std::max(length(toLamp), 1.0e-6);
        const Vec3 lightDirection = toLamp / distance;
        const double ndl = std::clamp(std::abs(dot(normal, lightDirection)), 0.0, 1.0);
        const double attenuation = 1.0 / (1.0 + 0.42 * distance * distance);
        color += occluder.color * lamp.color *
                 (lamp.intensity * attenuation * (0.18 + 0.82 * ndl));
    }
    return toneMap(color);
}

void addEnergy(
    std::uint64_t& destination,
    double value) {
    destination += static_cast<std::uint64_t>(fixedEnergy(value));
}

SampleResult shadeSample(
    const Scene& scene,
    const TextureSet& textures,
    const ShadowSampler& shadowSampler,
    const std::vector<LayerHit>& hits,
    const OccluderHit& occluder,
    BlockStats& stats) {
    SampleResult result;
    double accumulatedAlpha = 0.0;
    Vec3 accumulatedColor{};
    double occluderLumaContribution = 0.0;

    for (const LayerHit& hit : hits) {
        if (hit.layerIndex < 0 ||
            hit.layerIndex >= static_cast<int>(scene.layers.size())) {
            continue;
        }
        if (occluder.valid && hit.depth > occluder.depth - 1.0e-7) continue;
        const Layer& layer = scene.layers[static_cast<std::size_t>(hit.layerIndex)];
        const auto textureIt = textures.layers.find(layer.id);
        if (textureIt == textures.layers.end()) continue;
        FragmentShade shade = shadeLayer(
            scene,
            layer,
            textureIt->second,
            hit.world,
            hit.uv,
            hit.normal,
            shadowSampler);
        const double weight = 1.0 - accumulatedAlpha;
        const Vec3 contribution = shade.premultiplied * weight;
        accumulatedColor += contribution;
        accumulatedAlpha += shade.alpha * weight;
        result.covered = true;
        if (result.nearestLayer == 0) result.nearestLayer = layer.id;
        ++result.visibleLayers;
        result.layerContribution[static_cast<std::size_t>(layer.id)] +=
            luminance(contribution);

        const double rawLuma = luminance(
            shade.diffuse + shade.transmit + shade.reflect);
        const double contributionLuma = luminance(contribution);
        if (rawLuma > 1.0e-14) {
            const double transmitShare =
                luminance(shade.transmit) / rawLuma;
            const double reflectShare =
                luminance(shade.reflect) / rawLuma;
            const double diffuseShare =
                luminance(shade.diffuse) / rawLuma;
            const std::size_t index = static_cast<std::size_t>(hit.layerIndex);
            addEnergy(stats.transmitEnergy[index], contributionLuma * transmitShare);
            addEnergy(stats.reflectEnergy[index], contributionLuma * reflectShare);
            addEnergy(stats.diffuseEnergy[index], contributionLuma * diffuseShare);
            stats.layerSamples[index] += 1;
        }
        stats.woodSamples += shade.woodWeight > 0.5 ? 1U : 0U;
        stats.gauzeSamples += shade.gauzeWeight > 0.5 ? 1U : 0U;
    }

    if (occluder.valid) {
        const Occluder& part =
            scene.occluders[static_cast<std::size_t>(occluder.occluderIndex)];
        const Vec3 color = shadeOccluder(scene, part, occluder.world, occluder.normal);
        const double weight = 1.0 - accumulatedAlpha;
        accumulatedColor += color * weight;
        occluderLumaContribution = luminance(color * weight);
        accumulatedAlpha = 1.0;
        result.covered = true;
        if (result.nearestLayer == 0) result.nearestLayer = 255;
        ++result.visibleLayers;
        ++stats.occluderSamples;
    }

    if (result.nearestLayer == 0) {
        result.nearestLayer = 255;
    }
    if (result.covered) ++stats.coveredSamples;
    result.premultiplied = accumulatedColor;
    result.alpha = accumulatedAlpha;
    result.occluderContribution = occluderLumaContribution;
    return result;
}

}  // namespace

void BlockStats::resize(std::size_t layerCount) {
    transmitEnergy.assign(layerCount, 0U);
    reflectEnergy.assign(layerCount, 0U);
    diffuseEnergy.assign(layerCount, 0U);
    layerSamples.assign(layerCount, 0U);
}

BlockRect blockRect(const Scene& scene, int blockIndex) {
    const int columns = scene.blockColumns();
    BlockRect rect;
    rect.index = blockIndex;
    rect.x = (blockIndex % columns) * scene.image.tileSize;
    rect.y = (blockIndex / columns) * scene.image.tileSize;
    rect.width = std::min(scene.image.tileSize, scene.image.width - rect.x);
    rect.height = std::min(scene.image.tileSize, scene.image.height - rect.y);
    return rect;
}

Renderer::Renderer(
    const Scene& scene,
    const TextureSet& textures,
    const ShadowSampler& shadowSampler)
    : scene_(scene), textures_(textures), shadowSampler_(shadowSampler) {}

BlockResult Renderer::renderBlock(int blockIndex) const {
    const BlockRect rect = blockRect(scene_, blockIndex);
    const int aa = scene_.image.aaSamples;
    const int fullSampleWidth = scene_.image.width * aa;
    const int fullSampleHeight = scene_.image.height * aa;
    const int sampleWidth = rect.width * aa;
    const int sampleHeight = rect.height * aa;
    const int originSampleX = rect.x * aa;
    const int originSampleY = rect.y * aa;
    const std::size_t sampleCount =
        static_cast<std::size_t>(sampleWidth) * static_cast<std::size_t>(sampleHeight);

    std::vector<std::vector<LayerHit>> hitLists(sampleCount);
    std::vector<OccluderHit> occluderHits(sampleCount);

    const auto rasterizeTriangle = [&](const std::array<CameraVertex, 3>& input,
                                       int layerIndex,
                                       int quadrant,
                                       int triangleIndex,
                                       int occluderIndex) {
        std::vector<CameraVertex> polygon = clipNear(input, scene_.camera.nearZ);
        if (polygon.size() < 3) return;
        for (std::size_t fan = 1; fan + 1 < polygon.size(); ++fan) {
            std::array<CameraVertex, 3> clipped = {
                polygon[0], polygon[fan], polygon[fan + 1]};
            std::array<ScreenVertex, 3> screen = {
                project(clipped[0], scene_.camera, fullSampleWidth, fullSampleHeight),
                project(clipped[1], scene_.camera, fullSampleWidth, fullSampleHeight),
                project(clipped[2], scene_.camera, fullSampleWidth, fullSampleHeight),
            };
            const double denominator = edgeFunction(screen[0], screen[1], screen[2].x, screen[2].y);
            if (std::abs(denominator) <= 1.0e-12) continue;
            const double inverseDenominator = 1.0 / denominator;
            const double minX = std::min({screen[0].x, screen[1].x, screen[2].x});
            const double maxX = std::max({screen[0].x, screen[1].x, screen[2].x});
            const double minY = std::min({screen[0].y, screen[1].y, screen[2].y});
            const double maxY = std::max({screen[0].y, screen[1].y, screen[2].y});
            const int startX = std::max(originSampleX, static_cast<int>(std::floor(minX - 0.5)));
            const int endX = std::min(
                originSampleX + sampleWidth - 1,
                static_cast<int>(std::ceil(maxX + 0.5)));
            const int startY = std::max(originSampleY, static_cast<int>(std::floor(minY - 0.5)));
            const int endY = std::min(
                originSampleY + sampleHeight - 1,
                static_cast<int>(std::ceil(maxY + 0.5)));
            if (startX > endX || startY > endY) continue;
            const Vec3 worldNormal = triangleNormal(
                clipped[0].world, clipped[1].world, clipped[2].world);
            if (lengthSquared(worldNormal) <= 1.0e-20) continue;

            for (int sampleY = startY; sampleY <= endY; ++sampleY) {
                for (int sampleX = startX; sampleX <= endX; ++sampleX) {
                    const double px = static_cast<double>(sampleX) + 0.5;
                    const double py = static_cast<double>(sampleY) + 0.5;
                    const double w0 = edgeFunction(screen[1], screen[2], px, py) *
                                      inverseDenominator;
                    const double w1 = edgeFunction(screen[2], screen[0], px, py) *
                                      inverseDenominator;
                    const double w2 = 1.0 - w0 - w1;
                    constexpr double edgeTolerance = 1.0e-9;
                    if (w0 < -edgeTolerance || w1 < -edgeTolerance ||
                        w2 < -edgeTolerance) {
                        continue;
                    }
                    const double invDepth =
                        w0 / screen[0].depth + w1 / screen[1].depth +
                        w2 / screen[2].depth;
                    if (invDepth <= 0.0) continue;
                    const double depth = 1.0 / invDepth;
                    const Vec3 world =
                        (screen[0].world * (w0 / screen[0].depth) +
                         screen[1].world * (w1 / screen[1].depth) +
                         screen[2].world * (w2 / screen[2].depth)) *
                        depth;
                    const Vec2 uv =
                        (screen[0].uv * (w0 / screen[0].depth) +
                         screen[1].uv * (w1 / screen[1].depth) +
                         screen[2].uv * (w2 / screen[2].depth)) *
                        depth;
                    const int tileSampleX = sampleX - originSampleX;
                    const int tileSampleY = sampleY - originSampleY;
                    const std::size_t sampleIndex =
                        static_cast<std::size_t>(tileSampleY) *
                            static_cast<std::size_t>(sampleWidth) +
                        static_cast<std::size_t>(tileSampleX);
                    if (layerIndex >= 0) {
                        hitLists[sampleIndex].push_back({
                            layerIndex,
                            quadrant,
                            triangleIndex,
                            depth,
                            world,
                            uv,
                            worldNormal,
                        });
                    } else {
                        OccluderHit& current = occluderHits[sampleIndex];
                        const int stableKey = occluderIndex * 1000000 + triangleIndex;
                        const int currentKey =
                            current.occluderIndex * 1000000 + current.triangleIndex;
                        if (!current.valid || depth < current.depth - 1.0e-9 ||
                            (std::abs(depth - current.depth) <= 1.0e-9 &&
                             stableKey < currentKey)) {
                            current.valid = true;
                            current.occluderIndex = occluderIndex;
                            current.triangleIndex = triangleIndex;
                            current.depth = depth;
                            current.world = world;
                            current.normal = worldNormal;
                        }
                    }
                }
            }
        }
    };

    for (std::size_t layerIndex = 0; layerIndex < scene_.layers.size(); ++layerIndex) {
        const Layer& layer = scene_.layers[layerIndex];
        for (const QuadrantMesh& quadrant : layer.quadrants) {
            const Mesh& mesh = quadrant.mesh;
            for (std::size_t i = 0, triangle = 0;
                 i < mesh.indices.size();
                 i += 3, ++triangle) {
                std::array<CameraVertex, 3> triangleVertices;
                for (int corner = 0; corner < 3; ++corner) {
                    const std::uint32_t vertexIndex =
                        mesh.indices[i + static_cast<std::size_t>(corner)];
                    triangleVertices[static_cast<std::size_t>(corner)] = toCamera(
                        mesh.positions[vertexIndex],
                        mesh.uvs[vertexIndex],
                        scene_.camera);
                }
                rasterizeTriangle(
                    triangleVertices,
                    static_cast<int>(layerIndex),
                    quadrant.quadrant,
                    static_cast<int>(triangle),
                    -1);
            }
        }
    }
    for (std::size_t occluderIndex = 0;
         occluderIndex < scene_.occluders.size();
         ++occluderIndex) {
        const Mesh& mesh = scene_.occluders[occluderIndex].mesh;
        for (std::size_t i = 0, triangle = 0; i < mesh.indices.size(); i += 3, ++triangle) {
            std::array<CameraVertex, 3> triangleVertices;
            for (int corner = 0; corner < 3; ++corner) {
                const std::uint32_t vertexIndex =
                    mesh.indices[i + static_cast<std::size_t>(corner)];
                triangleVertices[static_cast<std::size_t>(corner)] = toCamera(
                    mesh.positions[vertexIndex],
                    mesh.uvs[vertexIndex],
                    scene_.camera);
            }
            rasterizeTriangle(
                triangleVertices,
                -1,
                -1,
                static_cast<int>(triangle),
                static_cast<int>(occluderIndex));
        }
    }

    const int layerCount = static_cast<int>(scene_.layers.size());
    BlockResult result;
    result.rect = rect;
    result.stats.resize(scene_.layers.size());
    result.pixels.assign(
        static_cast<std::size_t>(rect.width) * static_cast<std::size_t>(rect.height) * 8,
        0U);

    for (int pixelY = 0; pixelY < rect.height; ++pixelY) {
        for (int pixelX = 0; pixelX < rect.width; ++pixelX) {
            PixelAccumulator accumulator;
            for (int subY = 0; subY < aa; ++subY) {
                for (int subX = 0; subX < aa; ++subX) {
                    const int sampleX = pixelX * aa + subX;
                    const int sampleY = pixelY * aa + subY;
                    const std::size_t sampleIndex =
                        static_cast<std::size_t>(sampleY) *
                            static_cast<std::size_t>(sampleWidth) +
                        static_cast<std::size_t>(sampleX);
                    std::vector<LayerHit>& hits = hitLists[sampleIndex];
                    std::sort(
                        hits.begin(),
                        hits.end(),
                        [](const LayerHit& a, const LayerHit& b) {
                            if (a.layerIndex != b.layerIndex) {
                                return a.layerIndex < b.layerIndex;
                            }
                            if (a.depth != b.depth) return a.depth < b.depth;
                            if (a.quadrant != b.quadrant) {
                                return a.quadrant < b.quadrant;
                            }
                            return a.triangleIndex < b.triangleIndex;
                        });
                    std::vector<LayerHit> uniqueHits;
                    uniqueHits.reserve(hits.size());
                    for (const LayerHit& hit : hits) {
                        if (!uniqueHits.empty() &&
                            uniqueHits.back().layerIndex == hit.layerIndex) {
                            continue;
                        }
                        uniqueHits.push_back(hit);
                    }
                    const SampleResult sample = shadeSample(
                        scene_,
                        textures_,
                        shadowSampler_,
                        uniqueHits,
                        occluderHits[sampleIndex],
                        result.stats);
                    accumulator.premulSum += sample.premultiplied;
                    accumulator.alphaSum += sample.alpha;
                    if (sample.covered) {
                        ++accumulator.coveredCount;
                        ++accumulator.nearestVotes[
                            static_cast<std::size_t>(sample.nearestLayer)];
                        accumulator.visibleLayerCountSum += sample.visibleLayers;
                    }
                    for (int id = 1; id <= layerCount; ++id) {
                        accumulator.layerWeights[static_cast<std::size_t>(id)] +=
                            sample.layerContribution[static_cast<std::size_t>(id)];
                    }
                    accumulator.occluderWeight += sample.occluderContribution;
                }
            }

            const int sampleTotal = aa * aa;
            const Vec3 premultiplied = accumulator.premulSum / static_cast<double>(sampleTotal);
            const double alpha = accumulator.alphaSum / static_cast<double>(sampleTotal);
            int nearest = 0;
            int nearestVotes = 0;
            for (int id = 1; id < 256; ++id) {
                const int votes = accumulator.nearestVotes[static_cast<std::size_t>(id)];
                if (votes > nearestVotes) {
                    nearestVotes = votes;
                    nearest = id;
                }
            }
            int dominant = 0;
            double dominantWeight = 0.0;
            for (int id = 1; id <= layerCount; ++id) {
                const double weight =
                    accumulator.layerWeights[static_cast<std::size_t>(id)];
                if (weight > dominantWeight + 1.0e-15) {
                    dominantWeight = weight;
                    dominant = id;
                }
            }
            if (accumulator.occluderWeight > dominantWeight + 1.0e-15) {
                dominant = 255;
            }
            const int visibleCount = accumulator.coveredCount > 0
                ? static_cast<int>(std::lround(
                      static_cast<double>(accumulator.visibleLayerCountSum) /
                      static_cast<double>(accumulator.coveredCount)))
                : 0;
            const int coverage = static_cast<int>(std::lround(
                static_cast<double>(accumulator.coveredCount) /
                static_cast<double>(sampleTotal) * 255.0));
            const auto byte = [](double value) -> std::uint8_t {
                return static_cast<std::uint8_t>(std::lround(
                    std::clamp(value, 0.0, 1.0) * 255.0));
            };
            const std::size_t pixelOffset =
                (static_cast<std::size_t>(pixelY) *
                     static_cast<std::size_t>(rect.width) +
                 static_cast<std::size_t>(pixelX)) *
                8;
            result.pixels[pixelOffset] = byte(premultiplied.x);
            result.pixels[pixelOffset + 1] = byte(premultiplied.y);
            result.pixels[pixelOffset + 2] = byte(premultiplied.z);
            result.pixels[pixelOffset + 3] = byte(alpha);
            result.pixels[pixelOffset + 4] = static_cast<std::uint8_t>(nearest);
            result.pixels[pixelOffset + 5] = static_cast<std::uint8_t>(
                std::clamp(visibleCount, 0, 255));
            result.pixels[pixelOffset + 6] = static_cast<std::uint8_t>(dominant);
            result.pixels[pixelOffset + 7] = static_cast<std::uint8_t>(
                std::clamp(coverage, 0, 255));
        }
    }
    return result;
}

}  // namespace caisson

