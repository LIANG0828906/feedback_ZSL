#include "render.hpp"

#include "sha256.hpp"

#include <algorithm>
#include <array>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <iomanip>
#include <limits>
#include <map>
#include <numeric>
#include <queue>
#include <sstream>
#include <string>
#include <tuple>
#include <utility>
#include <vector>

namespace mosaic {

namespace {

constexpr double kCausticScale = 1.0e9;
constexpr double kStatsScale = 1.0e6;
constexpr std::uint32_t kTextureVersion = 1;
constexpr std::uint32_t kCausticBinaryVersion = 1;
constexpr std::uint16_t kStripeThreshold = 1000;

struct Ray {
    Vec3 origin;
    Vec3 direction;
};

struct Hit {
    std::size_t pieceIndex = 0;
    std::size_t triangleIndex = 0;
    double distance = 0.0;
    Vec3 point{};
    Vec3 normal{};
};

struct FloorHit {
    double distance = 0.0;
    Vec3 point{};
    Vec3 normal{};
};

struct TextureSample {
    Rgb transmission{};
    double ior = 1.0;
    double bubble = 0.0;
};

struct Component {
    Rgb premultiplied{};
    double alpha = 0.0;
};

struct PixelShade {
    Rgb premultiplied{};
    double alpha = 1.0;
    std::uint64_t owner = kNoOwnerKey;
    std::int32_t ownerLayer = -1;
};

void appendU16(std::vector<std::uint8_t>& out, std::uint16_t value) {
    out.push_back(static_cast<std::uint8_t>(value & 0xFFU));
    out.push_back(static_cast<std::uint8_t>((value >> 8U) & 0xFFU));
}

void appendU32(std::vector<std::uint8_t>& out, std::uint32_t value) {
    for (std::uint32_t shift = 0; shift < 32U; shift += 8U) {
        out.push_back(static_cast<std::uint8_t>((value >> shift) & 0xFFU));
    }
}

void appendU64(std::vector<std::uint8_t>& out, std::uint64_t value) {
    for (std::uint32_t shift = 0; shift < 64U; shift += 8U) {
        out.push_back(static_cast<std::uint8_t>((value >> shift) & 0xFFU));
    }
}

void appendDouble(std::vector<std::uint8_t>& out, double value) {
    const std::uint64_t bits = std::bit_cast<std::uint64_t>(value);
    appendU64(out, bits);
}

std::uint64_t saturatingAdd(std::uint64_t left, std::uint64_t right) {
    const std::uint64_t maximum = std::numeric_limits<std::uint64_t>::max();
    return right > maximum - left ? maximum : left + right;
}

std::uint64_t microValue(double value) {
    if (!(value > 0.0)) {
        return 0;
    }
    const double scaled = std::round(value * kStatsScale);
    if (scaled >= static_cast<double>(std::numeric_limits<std::uint64_t>::max())) {
        return std::numeric_limits<std::uint64_t>::max();
    }
    return static_cast<std::uint64_t>(scaled);
}

Rgb huePreservingCap(Rgb color) {
    color = clamp(color, 0.0, std::numeric_limits<double>::max());
    const double maximum = std::max({color.r, color.g, color.b});
    if (maximum > 1.0) {
        color = color / maximum;
    }
    return clamp(color, 0.0, 1.0);
}

std::uint8_t toByte(double value) {
    const double clamped = std::clamp(value, 0.0, 1.0);
    return static_cast<std::uint8_t>(std::lround(clamped * 255.0));
}

Ray cameraRay(const CameraFrame& camera, std::uint32_t pixelX, std::uint32_t pixelY) {
    const double ndcX = (static_cast<double>(pixelX) + 0.5) /
                            static_cast<double>(camera.width) * 2.0 -
                        1.0;
    const double ndcY = 1.0 -
                        (static_cast<double>(pixelY) + 0.5) /
                            static_cast<double>(camera.height) * 2.0;
    const Vec3 direction = normalize(
        camera.forward + camera.right * (ndcX * camera.tanHalfFov * camera.aspect) +
        camera.up * (ndcY * camera.tanHalfFov));
    return {camera.position, direction};
}

ProjectedPoint projectPoint(const CameraFrame& camera, const Vec3& point) {
    const Vec3 relative = point - camera.position;
    const double depth = dot(relative, camera.forward);
    if (depth <= 1.0e-7) {
        return {-1.0, -1.0, depth, false};
    }
    const double x = dot(relative, camera.right) / (depth * camera.tanHalfFov * camera.aspect);
    const double y = dot(relative, camera.up) / (depth * camera.tanHalfFov);
    const double pixelX = (x * 0.5 + 0.5) * static_cast<double>(camera.width) - 0.5;
    const double pixelY = (0.5 - y * 0.5) * static_cast<double>(camera.height) - 0.5;
    const bool visible = pixelX >= -1.0 && pixelY >= -1.0 &&
                         pixelX <= static_cast<double>(camera.width) &&
                         pixelY <= static_cast<double>(camera.height);
    return {pixelX, pixelY, depth, visible};
}

bool rayTriangle(
    const Ray& ray,
    const Vec3& a,
    const Vec3& b,
    const Vec3& c,
    double& distance,
    Vec3& point) {
    const Vec3 edge1 = b - a;
    const Vec3 edge2 = c - a;
    const Vec3 p = cross(ray.direction, edge2);
    const double determinant = dot(edge1, p);
    if (std::abs(determinant) <= 1.0e-12) {
        return false;
    }
    const double inverse = 1.0 / determinant;
    const Vec3 t = ray.origin - a;
    const double u = dot(t, p) * inverse;
    if (u < -1.0e-9 || u > 1.0 + 1.0e-9) {
        return false;
    }
    const Vec3 q = cross(t, edge1);
    const double v = dot(ray.direction, q) * inverse;
    if (v < -1.0e-9 || u + v > 1.0 + 1.0e-9) {
        return false;
    }
    distance = dot(edge2, q) * inverse;
    if (distance <= 1.0e-8) {
        return false;
    }
    point = ray.origin + ray.direction * distance;
    return true;
}

double pointSegmentDistance(const Vec3& point, const Vec3& a, const Vec3& b) {
    const Vec3 ab = b - a;
    const double denominator = lengthSquared(ab);
    if (denominator <= 1.0e-24) {
        return length(point - a);
    }
    const double t = std::clamp(dot(point - a, ab) / denominator, 0.0, 1.0);
    return length(point - (a + ab * t));
}

bool pointInTriangle2D(
    const Vec3& point,
    const Vec3& a,
    const Vec3& b,
    const Vec3& c) {
    const auto sign = [](const Vec3& p1, const Vec3& p2, const Vec3& p3) {
        return (p1.x - p3.x) * (p2.z - p3.z) - (p2.x - p3.x) * (p1.z - p3.z);
    };
    const double d1 = sign(point, a, b);
    const double d2 = sign(point, b, c);
    const double d3 = sign(point, c, a);
    const bool hasNegative = d1 < -1.0e-12 || d2 < -1.0e-12 || d3 < -1.0e-12;
    const bool hasPositive = d1 > 1.0e-12 || d2 > 1.0e-12 || d3 > 1.0e-12;
    return !(hasNegative && hasPositive);
}

std::vector<BoundaryEdge> buildBoundaryEdges(const Mesh& mesh) {
    std::map<std::pair<std::uint32_t, std::uint32_t>, std::uint32_t> counts;
    for (const Triangle& triangle : mesh.triangles) {
        for (std::size_t i = 0; i < 3; ++i) {
            std::uint32_t first = triangle.vertices[i];
            std::uint32_t second = triangle.vertices[(i + 1) % 3];
            if (first > second) {
                std::swap(first, second);
            }
            ++counts[{first, second}];
        }
    }
    std::vector<BoundaryEdge> edges;
    for (const auto& [edge, count] : counts) {
        if (count == 1U) {
            edges.push_back({edge.first, edge.second});
        }
    }
    return edges;
}

PieceTexture generateTexture(const Scene& scene, const Piece& piece) {
    std::uint64_t key = splitmix64(scene.seed ^ 0xa0761d6478bd642fULL);
    key = hashCombine(key, static_cast<std::uint64_t>(piece.layerIndex));
    key = hashCombine(key, piece.pieceId);
    key = hashCombine(key, 0xe7037ed1a0b428dbULL);

    PieceTexture texture;
    texture.transmissionScale = {
        0.94 + 0.12 * unitDouble(splitmix64(key + 1U)),
        0.94 + 0.12 * unitDouble(splitmix64(key + 2U)),
        0.94 + 0.12 * unitDouble(splitmix64(key + 3U)),
    };
    texture.iorScale = 0.995 + 0.01 * unitDouble(splitmix64(key + 4U));

    auto next = [&key]() {
        key = splitmix64(key);
        return key;
    };

    texture.bubbles.reserve(scene.settings.texture.bubblesPerPiece);
    for (std::uint32_t i = 0; i < scene.settings.texture.bubblesPerPiece; ++i) {
        const double angle = unitDouble(next()) * 2.0 * kPi;
        const double radius = std::sqrt(unitDouble(next())) * 0.42;
        TextureBubble bubble;
        bubble.centerU = 0.5 + std::cos(angle) * radius;
        bubble.centerV = 0.5 + std::sin(angle) * radius;
        bubble.radius = 0.018 + 0.055 * unitDouble(next());
        bubble.strength = 0.12 + 0.46 * unitDouble(next());
        bubble.iorDelta = -0.025 + 0.05 * unitDouble(next());
        bubble.tint = {
            -0.09 + 0.18 * unitDouble(next()),
            -0.09 + 0.18 * unitDouble(next()),
            -0.09 + 0.18 * unitDouble(next()),
        };
        texture.bubbles.push_back(bubble);
    }

    texture.flows.reserve(scene.settings.texture.flowBandsPerPiece);
    for (std::uint32_t i = 0; i < scene.settings.texture.flowBandsPerPiece; ++i) {
        const double angle = unitDouble(next()) * 2.0 * kPi;
        TextureFlow flow;
        flow.directionU = std::cos(angle);
        flow.directionV = std::sin(angle);
        flow.frequency = 0.75 + 3.5 * unitDouble(next());
        flow.phase = unitDouble(next());
        flow.amplitude = 0.012 + 0.055 * unitDouble(next());
        flow.colorPhase = unitDouble(next());
        texture.flows.push_back(flow);
    }
    return texture;
}

TextureTable buildTextureTable(const PreparedScene& prepared) {
    std::vector<std::uint8_t> bytes;
    bytes.insert(bytes.end(), {'M', 'G', 'S', 'T'});
    appendU32(bytes, kTextureVersion);
    appendU64(bytes, prepared.scene->seed);
    appendU32(bytes, static_cast<std::uint32_t>(prepared.pieces.size()));
    appendU32(bytes, prepared.scene->settings.texture.bubblesPerPiece);
    appendU32(bytes, prepared.scene->settings.texture.flowBandsPerPiece);
    for (const PreparedPiece& piece : prepared.pieces) {
        appendU32(bytes, static_cast<std::uint32_t>(piece.piece->layerIndex));
        appendU32(bytes, piece.piece->pieceId);
        appendDouble(bytes, piece.texture.transmissionScale.r);
        appendDouble(bytes, piece.texture.transmissionScale.g);
        appendDouble(bytes, piece.texture.transmissionScale.b);
        appendDouble(bytes, piece.texture.iorScale);
        appendU32(bytes, static_cast<std::uint32_t>(piece.texture.bubbles.size()));
        for (const TextureBubble& bubble : piece.texture.bubbles) {
            appendDouble(bytes, bubble.centerU);
            appendDouble(bytes, bubble.centerV);
            appendDouble(bytes, bubble.radius);
            appendDouble(bytes, bubble.strength);
            appendDouble(bytes, bubble.iorDelta);
            appendDouble(bytes, bubble.tint.r);
            appendDouble(bytes, bubble.tint.g);
            appendDouble(bytes, bubble.tint.b);
        }
        appendU32(bytes, static_cast<std::uint32_t>(piece.texture.flows.size()));
        for (const TextureFlow& flow : piece.texture.flows) {
            appendDouble(bytes, flow.directionU);
            appendDouble(bytes, flow.directionV);
            appendDouble(bytes, flow.frequency);
            appendDouble(bytes, flow.phase);
            appendDouble(bytes, flow.amplitude);
            appendDouble(bytes, flow.colorPhase);
        }
    }
    return {bytes, Sha256::digest(bytes)};
}

TextureSample sampleTexture(const PreparedPiece& piece, const Vec3& point) {
    const Vec3 extent = piece.boundingMax - piece.boundingMin;
    const double width = std::max(extent.x, 1.0e-9);
    const double depth = std::max(extent.z, 1.0e-9);
    const double u = std::clamp((point.x - piece.boundingMin.x) / width, 0.0, 1.0);
    const double v = std::clamp((point.z - piece.boundingMin.z) / depth, 0.0, 1.0);

    Rgb flowModulation{1.0, 1.0, 1.0};
    double iorShift = 0.0;
    for (const TextureFlow& flow : piece.texture.flows) {
        const double argument =
            flow.phase + u * flow.directionU * flow.frequency +
            v * flow.directionV * flow.frequency;
        const double base = std::sin(2.0 * kPi * argument);
        flowModulation.r *= 1.0 + flow.amplitude * std::sin(
            2.0 * kPi * (argument + flow.colorPhase));
        flowModulation.g *= 1.0 + flow.amplitude * std::sin(
            2.0 * kPi * (argument + flow.colorPhase + 1.0 / 3.0));
        flowModulation.b *= 1.0 + flow.amplitude * std::sin(
            2.0 * kPi * (argument + flow.colorPhase + 2.0 / 3.0));
        iorShift += flow.amplitude * base * 0.08;
    }

    double bubbleDensity = 0.0;
    Rgb bubbleTint{};
    double bubbleIor = 0.0;
    for (const TextureBubble& bubble : piece.texture.bubbles) {
        const double du = u - bubble.centerU;
        const double dv = v - bubble.centerV;
        const double distance = std::sqrt(du * du + dv * dv);
        if (distance >= bubble.radius) {
            continue;
        }
        const double radial = 1.0 - distance / bubble.radius;
        const double falloff = radial * radial * (3.0 - 2.0 * radial);
        bubbleDensity += bubble.strength * falloff;
        bubbleTint += bubble.tint * (bubble.strength * falloff);
        bubbleIor += bubble.iorDelta * falloff;
    }

    TextureSample sample;
    const Rgb base = piece.piece->transmission * piece.texture.transmissionScale;
    sample.transmission = clamp(
        base * flowModulation + bubbleTint * 0.16,
        0.0,
        1.0);
    sample.ior = std::clamp(
        piece.piece->edgeIor * piece.texture.iorScale + iorShift + bubbleIor,
        1.0,
        3.0);
    sample.bubble = std::clamp(bubbleDensity, 0.0, 1.5);
    return sample;
}

double fresnelSchlick(double cosine, double ior) {
    const double ratio = (ior - 1.0) / (ior + 1.0);
    const double f0 = ratio * ratio;
    return std::clamp(f0 + (1.0 - f0) * std::pow(1.0 - cosine, 5.0), 0.0, 1.0);
}

double smootherstep(double edge0, double edge1, double value) {
    if (edge1 <= edge0) {
        return value >= edge1 ? 1.0 : 0.0;
    }
    const double t = std::clamp((value - edge0) / (edge1 - edge0), 0.0, 1.0);
    return t * t * (3.0 - 2.0 * t);
}

double boundaryDistance(const PreparedPiece& piece, const Vec3& point) {
    double minimum = std::numeric_limits<double>::max();
    for (const BoundaryEdge& edge : piece.boundaryEdges) {
        minimum = std::min(
            minimum,
            pointSegmentDistance(
                point,
                piece.piece->mesh.vertices[edge.first],
                piece.piece->mesh.vertices[edge.second]));
    }
    if (!std::isfinite(minimum)) {
        return piece.characteristicSize;
    }
    return minimum;
}

Component overComponent(const Component& top, const Component& bottom) {
    Component result;
    result.premultiplied =
        top.premultiplied + bottom.premultiplied * (1.0 - top.alpha);
    result.alpha = top.alpha + bottom.alpha * (1.0 - top.alpha);
    return result;
}

Component makeComponent(Rgb unpremultiplied, double alpha) {
    alpha = std::clamp(alpha, 0.0, 1.0);
    Component component;
    component.alpha = alpha;
    component.premultiplied = huePreservingCap(unpremultiplied) * alpha;
    return component;
}

std::array<std::uint64_t, 3> shadePieceHit(
    const PreparedScene& prepared,
    const Hit& hit,
    const Ray& cameraRayValue,
    Component& outputComponent) {
    const PreparedPiece& preparedPiece = prepared.pieces[hit.pieceIndex];
    const Piece& piece = *preparedPiece.piece;
    const TextureSample texture = sampleTexture(preparedPiece, hit.point);
    const double edgeDistance = boundaryDistance(preparedPiece, hit.point);
    const double edgeThreshold =
        std::max(preparedPiece.characteristicSize * prepared.scene->settings.caustic.edgeBand, 1.0e-6);
    const double edgeWeight = 1.0 - smootherstep(0.0, edgeThreshold, edgeDistance);

    Rgb transmission{};
    Rgb reflection{};
    Rgb transmissionUnpremultiplied{};
    Rgb reflectionUnpremultiplied{};
    for (const Light& light : prepared.scene->lights) {
        const Vec3 toLight = normalize(light.position - hit.point);
        const double cosine = std::clamp(std::abs(dot(hit.normal, toLight)), 0.0, 1.0);
        const double pathLength = piece.thickness / std::max(cosine, 0.22);
        const double viewDiffuse = 0.48 + 0.24 * cosine;
        const Rgb lightRadiance = light.color * light.intensity;

        Rgb attenuation{};
        attenuation.r = std::exp(-(1.0 - texture.transmission.r) * 45.0 * pathLength);
        attenuation.g = std::exp(-(1.0 - texture.transmission.g) * 45.0 * pathLength);
        attenuation.b = std::exp(-(1.0 - texture.transmission.b) * 45.0 * pathLength);

        Rgb fresnel{};
        fresnel.r = fresnelSchlick(cosine, texture.ior * 0.985);
        fresnel.g = fresnelSchlick(cosine, texture.ior);
        fresnel.b = fresnelSchlick(cosine, texture.ior * 1.015);
        const double lowAngle = std::pow(1.0 - cosine, prepared.scene->settings.glare.edgePower);
        const double iorGlow = std::clamp((texture.ior - 1.0) * 1.5, 0.0, 1.0);

        const Rgb transmitted =
            lightRadiance * texture.transmission * attenuation * viewDiffuse;
        const Rgb scatter =
            lightRadiance * texture.transmission * (texture.bubble * 0.095);
        transmission += transmitted + scatter;

        Rgb reflected = lightRadiance * fresnel * (0.20 + 0.80 * edgeWeight);
        reflected *= prepared.scene->settings.glare.reflectionScale *
                     (0.18 + 0.82 * lowAngle) * (0.75 + 0.25 * iorGlow);
        reflection += reflected;

        transmissionUnpremultiplied += transmitted;
        reflectionUnpremultiplied += reflected;
    }

    Rgb tintedTransmission = texture.transmission * 0.05;
    transmission += tintedTransmission;
    transmissionUnpremultiplied += tintedTransmission;

    const double transmissionAlpha = std::clamp(
        0.20 + 0.64 * luminance(texture.transmission) +
            0.10 * texture.bubble,
        0.20,
        0.88);
    const double reflectionAlpha = std::clamp(
        0.10 + 1.35 * luminance(reflection),
        0.10,
        0.62);

    Component transComponent = makeComponent(transmission, transmissionAlpha);
    Component reflectComponent = makeComponent(reflection, reflectionAlpha);
    Component accumulated;
    for (const std::string& stage : prepared.scene->settings.compositionOrder) {
        if (stage == "transmission") {
            accumulated = overComponent(transComponent, accumulated);
        } else {
            accumulated = overComponent(reflectComponent, accumulated);
        }
    }
    outputComponent = accumulated;

    (void)cameraRayValue;
    const double transmissionEnergy = luminance(transComponent.premultiplied);
    const double reflectionEnergy = luminance(reflectComponent.premultiplied);
    return {
        microValue(transmissionEnergy),
        microValue(reflectionEnergy),
        microValue(luminance(texture.transmission)),
    };
}

FloorHit nearestFloorHit(const PreparedScene& prepared, const Ray& ray) {
    FloorHit result;
    result.distance = std::numeric_limits<double>::max();
    for (const Triangle& triangle : prepared.scene->floor.triangles) {
        const Vec3& a = prepared.scene->floor.vertices[triangle.vertices[0]];
        const Vec3& b = prepared.scene->floor.vertices[triangle.vertices[1]];
        const Vec3& c = prepared.scene->floor.vertices[triangle.vertices[2]];
        double distance = 0.0;
        Vec3 point;
        if (rayTriangle(ray, a, b, c, distance, point) && distance < result.distance) {
            Vec3 normal = normalize(cross(b - a, c - a));
            if (normal.y < 0.0) {
                normal = -normal;
            }
            result.distance = distance;
            result.point = point;
            result.normal = normal;
        }
    }
    return result;
}

PixelShade shadePixel(
    const PreparedScene& prepared,
    const CausticResult& caustics,
    std::uint32_t x,
    std::uint32_t y,
    std::vector<LayerStats>& chunkStats) {
    const Ray ray = cameraRay(prepared.camera, x, y);
    std::vector<Hit> hits;
    hits.reserve(8);
    for (std::size_t pieceIndex = 0; pieceIndex < prepared.pieces.size(); ++pieceIndex) {
        const PreparedPiece& preparedPiece = prepared.pieces[pieceIndex];
        const Mesh& mesh = preparedPiece.piece->mesh;
        for (std::size_t triangleIndex = 0; triangleIndex < mesh.triangles.size(); ++triangleIndex) {
            const Triangle& triangle = mesh.triangles[triangleIndex];
            const Vec3& a = mesh.vertices[triangle.vertices[0]];
            const Vec3& b = mesh.vertices[triangle.vertices[1]];
            const Vec3& c = mesh.vertices[triangle.vertices[2]];
            double distance = 0.0;
            Vec3 point;
            if (!rayTriangle(ray, a, b, c, distance, point)) {
                continue;
            }
            Vec3 normal = normalize(cross(b - a, c - a));
            if (normal.y < 0.0) {
                normal = -normal;
            }
            hits.push_back({pieceIndex, triangleIndex, distance, point, normal});
        }
    }

    std::sort(hits.begin(), hits.end(), [&](const Hit& left, const Hit& right) {
        const PreparedPiece& leftPiece = prepared.pieces[left.pieceIndex];
        const PreparedPiece& rightPiece = prepared.pieces[right.pieceIndex];
        if (leftPiece.piece->layerIndex != rightPiece.piece->layerIndex) {
            return leftPiece.piece->layerIndex < rightPiece.piece->layerIndex;
        }
        if (leftPiece.piece->pieceId != rightPiece.piece->pieceId) {
            return leftPiece.piece->pieceId < rightPiece.piece->pieceId;
        }
        if (left.distance != right.distance) {
            return left.distance < right.distance;
        }
        return left.triangleIndex < right.triangleIndex;
    });

    PixelShade shade;
    Component state;
    state.premultiplied = {0.026, 0.032, 0.050};
    state.alpha = 1.0;
    const FloorHit floorHit = nearestFloorHit(prepared, ray);
    if (floorHit.distance < std::numeric_limits<double>::max()) {
        Rgb floorColor{0.13, 0.105, 0.082};
        for (const Light& light : prepared.scene->lights) {
            const Vec3 toLight = normalize(light.position - floorHit.point);
            const double cosine = std::max(0.0, dot(floorHit.normal, toLight));
            floorColor += light.color * (light.intensity * 0.035 * cosine);
        }
        floorColor = huePreservingCap(floorColor);
        state.premultiplied = floorColor;
        state.alpha = 1.0;
        shade.owner = kFloorOwnerKey;
    }

    for (const Hit& hit : hits) {
        const PreparedPiece& preparedPiece = prepared.pieces[hit.pieceIndex];
        const Piece& piece = *preparedPiece.piece;
        Component component;
        const auto energy = shadePieceHit(prepared, hit, ray, component);
        state = overComponent(component, state);

        const auto layerIt = std::lower_bound(
            prepared.layers.begin(),
            prepared.layers.end(),
            piece.layerIndex);
        if (layerIt != prepared.layers.end()) {
            const std::size_t layerSlot =
                static_cast<std::size_t>(std::distance(prepared.layers.begin(), layerIt));
            if (layerSlot < chunkStats.size()) {
                LayerStats& stats = chunkStats[layerSlot];
                stats.visibleFragments += 1;
                stats.transmissionEnergyMicro = saturatingAdd(
                    stats.transmissionEnergyMicro,
                    energy[0]);
                stats.reflectionEnergyMicro = saturatingAdd(
                    stats.reflectionEnergyMicro,
                    energy[1]);
                stats.transmissionLuminanceMicro = saturatingAdd(
                    stats.transmissionLuminanceMicro,
                    energy[2]);
            }
        }

        const std::uint64_t key = ownershipKey(piece);
        if (piece.layerIndex > shade.ownerLayer ||
            (piece.layerIndex == shade.ownerLayer &&
             (shade.owner == kFloorOwnerKey || shade.owner == kNoOwnerKey ||
              key < shade.owner))) {
            shade.owner = key;
            shade.ownerLayer = piece.layerIndex;
        }
    }

    const std::size_t pixelIndex =
        static_cast<std::size_t>(y) * prepared.camera.width + x;
    const CausticPixel& causticPixel = caustics.accum[pixelIndex];
    const Rgb caustic =
        Rgb{
            static_cast<double>(causticPixel.r),
            static_cast<double>(causticPixel.g),
            static_cast<double>(causticPixel.b),
        } /
        kCausticScale;
    state.premultiplied = huePreservingCap(state.premultiplied + caustic);
    state.premultiplied = clamp(state.premultiplied, 0.0, 1.0);
    state.alpha = 1.0;
    shade.premultiplied = state.premultiplied;
    shade.alpha = state.alpha;
    return shade;
}

std::vector<std::uint8_t> encodeCausticRecords(
    const PreparedScene& prepared,
    const CausticResult& caustics) {
    std::vector<std::uint8_t> bytes;
    bytes.insert(bytes.end(), {'M', 'C', 'S', 'T'});
    appendU32(bytes, kCausticBinaryVersion);
    appendU32(bytes, prepared.scene->settings.output.width);
    appendU32(bytes, prepared.scene->settings.output.height);
    appendU32(bytes, static_cast<std::uint32_t>(caustics.records.size()));
    for (const CausticRecord& record : caustics.records) {
        appendU32(bytes, record.sampleIndex);
        appendU32(bytes, static_cast<std::uint32_t>(record.sourceLayer));
        appendU32(bytes, record.sourcePieceId);
        appendU32(bytes, record.lightId);
        appendDouble(bytes, record.sourcePoint.x);
        appendDouble(bytes, record.sourcePoint.y);
        appendDouble(bytes, record.sourcePoint.z);
        appendU32(bytes, static_cast<std::uint32_t>(record.targetKind));
        appendU32(bytes, static_cast<std::uint32_t>(record.targetLayer));
        appendU32(bytes, record.targetPieceId);
        appendDouble(bytes, record.targetPoint.x);
        appendDouble(bytes, record.targetPoint.y);
        appendDouble(bytes, record.targetPoint.z);
        appendDouble(bytes, record.thickness);
        appendDouble(bytes, record.layerDistance);
        appendDouble(bytes, record.penumbraSigmaWorld);
        appendDouble(bytes, record.penumbraSigmaPixels);
        appendDouble(bytes, record.screenX);
        appendDouble(bytes, record.screenY);
        appendDouble(bytes, record.peakBrightness);
        appendU32(bytes, record.outerGeometryHit ? 1U : 0U);
    }
    return bytes;
}

void addCausticSplat(
    const PreparedScene& prepared,
    CausticResult& result,
    const CausticRecord& record,
    const Rgb& color) {
    if (!record.outerGeometryHit && record.targetKind == CausticTargetKind::Floor) {
        // A missed outer tile intentionally falls back to the base plane; no occluder mask is applied.
    }
    if (!std::isfinite(record.screenX) || !std::isfinite(record.screenY) ||
        record.screenX < -1.0 || record.screenY < -1.0 ||
        record.screenX > static_cast<double>(prepared.camera.width) ||
        record.screenY > static_cast<double>(prepared.camera.height)) {
        return;
    }
    const double sigma = std::max(record.penumbraSigmaPixels, 0.35);
    const double support = prepared.scene->settings.caustic.splatSupport * sigma;
    const int minX = std::max(
        0,
        static_cast<int>(std::floor(record.screenX - support)));
    const int maxX = std::min(
        static_cast<int>(prepared.camera.width) - 1,
        static_cast<int>(std::ceil(record.screenX + support)));
    const int minY = std::max(
        0,
        static_cast<int>(std::floor(record.screenY - support)));
    const int maxY = std::min(
        static_cast<int>(prepared.camera.height) - 1,
        static_cast<int>(std::ceil(record.screenY + support)));
    const double inverseTwoSigmaSquared = 1.0 / (2.0 * sigma * sigma);
    for (int py = minY; py <= maxY; ++py) {
        for (int px = minX; px <= maxX; ++px) {
            const double dx = static_cast<double>(px) - record.screenX;
            const double dy = static_cast<double>(py) - record.screenY;
            const double distanceSquared = dx * dx + dy * dy;
            if (distanceSquared > support * support) {
                continue;
            }
            const double falloff = std::exp(-distanceSquared * inverseTwoSigmaSquared);
            const double energy = record.peakBrightness * falloff;
            const auto offset =
                (static_cast<std::size_t>(py) * prepared.camera.width +
                 static_cast<std::size_t>(px));
            CausticPixel& pixel = result.accum[offset];
            pixel.r = saturatingAdd(
                pixel.r,
                static_cast<std::uint64_t>(std::llround(color.r * energy * kCausticScale)));
            pixel.g = saturatingAdd(
                pixel.g,
                static_cast<std::uint64_t>(std::llround(color.g * energy * kCausticScale)));
            pixel.b = saturatingAdd(
                pixel.b,
                static_cast<std::uint64_t>(std::llround(color.b * energy * kCausticScale)));
        }
    }
}

std::uint32_t countCausticStripes(
    const std::vector<std::uint16_t>& gray,
    std::uint32_t width,
    std::uint32_t height) {
    std::vector<std::uint8_t> visited(gray.size(), 0U);
    std::uint32_t count = 0;
    std::queue<std::pair<std::uint32_t, std::uint32_t>> queue;
    for (std::uint32_t y = 0; y < height; ++y) {
        for (std::uint32_t x = 0; x < width; ++x) {
            const std::size_t index = static_cast<std::size_t>(y) * width + x;
            if (visited[index] != 0U || gray[index] < kStripeThreshold) {
                continue;
            }
            ++count;
            visited[index] = 1U;
            queue.push({x, y});
            while (!queue.empty()) {
                const auto [currentX, currentY] = queue.front();
                queue.pop();
                constexpr std::array<std::pair<int, int>, 4> offsets = {
                    std::pair{-1, 0},
                    std::pair{1, 0},
                    std::pair{0, -1},
                    std::pair{0, 1},
                };
                for (const auto [offsetX, offsetY] : offsets) {
                    const int nextX = static_cast<int>(currentX) + offsetX;
                    const int nextY = static_cast<int>(currentY) + offsetY;
                    if (nextX < 0 || nextY < 0 ||
                        nextX >= static_cast<int>(width) ||
                        nextY >= static_cast<int>(height)) {
                        continue;
                    }
                    const std::size_t nextIndex =
                        static_cast<std::size_t>(nextY) * width +
                        static_cast<std::size_t>(nextX);
                    if (visited[nextIndex] == 0U && gray[nextIndex] >= kStripeThreshold) {
                        visited[nextIndex] = 1U;
                        queue.push(
                            {static_cast<std::uint32_t>(nextX),
                             static_cast<std::uint32_t>(nextY)});
                    }
                }
            }
        }
    }
    return count;
}

std::uint16_t grayAt(
    const CausticResult& caustics,
    std::uint32_t width,
    std::uint32_t height,
    int x,
    int y) {
    if (x < 0 || y < 0 || x >= static_cast<int>(width) || y >= static_cast<int>(height)) {
        return 0;
    }
    return caustics.gray[static_cast<std::size_t>(y) * width + static_cast<std::size_t>(x)];
}

}  // namespace

PreparedScene prepareScene(const Scene& scene) {
    PreparedScene prepared;
    prepared.scene = &scene;
    prepared.camera.position = scene.camera.position;
    prepared.camera.forward = normalize(scene.camera.target - scene.camera.position);
    prepared.camera.right = normalize(cross(prepared.camera.forward, scene.camera.up));
    prepared.camera.up = normalize(cross(prepared.camera.right, prepared.camera.forward));
    prepared.camera.tanHalfFov = std::tan(scene.camera.fovYDeg * kPi / 360.0);
    prepared.camera.width = scene.settings.output.width;
    prepared.camera.height = scene.settings.output.height;
    prepared.camera.aspect =
        static_cast<double>(prepared.camera.width) /
        static_cast<double>(prepared.camera.height);
    prepared.floorTop = -std::numeric_limits<double>::max();
    for (const Vec3& vertex : scene.floor.vertices) {
        prepared.floorTop = std::max(prepared.floorTop, vertex.y);
    }
    if (!std::isfinite(prepared.floorTop)) {
        prepared.floorTop = 0.0;
    }

    prepared.pieces.reserve(scene.pieces.size());
    for (const Piece& piece : scene.pieces) {
        PreparedPiece item;
        item.piece = &piece;
        item.boundingMin = {
            std::numeric_limits<double>::max(),
            std::numeric_limits<double>::max(),
            std::numeric_limits<double>::max(),
        };
        item.boundingMax = {
            -std::numeric_limits<double>::max(),
            -std::numeric_limits<double>::max(),
            -std::numeric_limits<double>::max(),
        };
        Vec3 sum{};
        Vec3 normalSum{};
        for (const Vec3& vertex : piece.mesh.vertices) {
            item.boundingMin.x = std::min(item.boundingMin.x, vertex.x);
            item.boundingMin.y = std::min(item.boundingMin.y, vertex.y);
            item.boundingMin.z = std::min(item.boundingMin.z, vertex.z);
            item.boundingMax.x = std::max(item.boundingMax.x, vertex.x);
            item.boundingMax.y = std::max(item.boundingMax.y, vertex.y);
            item.boundingMax.z = std::max(item.boundingMax.z, vertex.z);
            sum += vertex;
        }
        if (!piece.mesh.vertices.empty()) {
            item.center = sum / static_cast<double>(piece.mesh.vertices.size());
        }
        for (const Triangle& triangle : piece.mesh.triangles) {
            const Vec3& a = piece.mesh.vertices[triangle.vertices[0]];
            const Vec3& b = piece.mesh.vertices[triangle.vertices[1]];
            const Vec3& c = piece.mesh.vertices[triangle.vertices[2]];
            Vec3 normal = normalize(cross(b - a, c - a));
            if (normal.y < 0.0) {
                normal = -normal;
            }
            normalSum += normal;
        }
        if (lengthSquared(normalSum) > 1.0e-18) {
            item.normal = normalize(normalSum);
            if (item.normal.y < 0.0) {
                item.normal = -item.normal;
            }
        }
        const Vec3 extent = item.boundingMax - item.boundingMin;
        item.characteristicSize = std::max({extent.x, extent.y, extent.z, 1.0e-6});
        item.boundaryEdges = buildBoundaryEdges(piece.mesh);
        item.texture = generateTexture(scene, piece);
        prepared.pieces.push_back(std::move(item));
    }

    for (const PreparedPiece& piece : prepared.pieces) {
        if (prepared.layers.empty() || prepared.layers.back() != piece.piece->layerIndex) {
            prepared.layers.push_back(piece.piece->layerIndex);
        }
    }
    if (!prepared.layers.empty()) {
        prepared.maximumLayer = prepared.layers.back();
    }
    prepared.textureTable = buildTextureTable(prepared);
    return prepared;
}

CausticResult renderCaustics(const PreparedScene& prepared) {
    CausticResult result;
    const std::uint32_t width = prepared.camera.width;
    const std::uint32_t height = prepared.camera.height;
    result.accum.assign(static_cast<std::size_t>(width) * height, {});

    std::map<std::int32_t, std::vector<std::size_t>> piecesByLayer;
    std::map<std::int32_t, double> layerHeight;
    for (std::size_t i = 0; i < prepared.pieces.size(); ++i) {
        piecesByLayer[prepared.pieces[i].piece->layerIndex].push_back(i);
        layerHeight[prepared.pieces[i].piece->layerIndex] += prepared.pieces[i].center.y;
    }
    for (auto& [layer, sum] : layerHeight) {
        const auto& pieces = piecesByLayer[layer];
        if (!pieces.empty()) {
            sum /= static_cast<double>(pieces.size());
        }
    }

    std::uint32_t sampleIndex = 0;
    for (const PreparedPiece& source : prepared.pieces) {
        const auto sourceLayer = source.piece->layerIndex;
        if (sourceLayer >= prepared.maximumLayer) {
            continue;
        }
        const auto nextLayerIt = std::upper_bound(
            prepared.layers.begin(), prepared.layers.end(), sourceLayer);
        if (nextLayerIt == prepared.layers.end()) {
            continue;
        }
        const std::int32_t targetLayer = *nextLayerIt;
        const auto targetPiecesIt = piecesByLayer.find(targetLayer);
        if (targetPiecesIt == piecesByLayer.end()) {
            continue;
        }
        const double targetY = layerHeight[targetLayer];
        const double sourceToTargetDistance = std::abs(targetY - source.center.y);
        const double sourceToFloorDistance = std::abs(source.center.y - prepared.floorTop);
        const std::uint64_t pieceSalt =
            hashCombine(hashCombine(prepared.scene->seed, static_cast<std::uint64_t>(sourceLayer)),
                        source.piece->pieceId);

        for (const BoundaryEdge& edge : source.boundaryEdges) {
            const Vec3 edgeA = source.piece->mesh.vertices[edge.first];
            const Vec3 edgeB = source.piece->mesh.vertices[edge.second];
            const double edgeLength = length(edgeB - edgeA);
            if (edgeLength <= 1.0e-9) {
                continue;
            }
            for (std::uint32_t sample = 0;
                 sample < prepared.scene->settings.caustic.samplesPerEdge;
                 ++sample) {
                const double t =
                    (static_cast<double>(sample) + 0.5) /
                    static_cast<double>(prepared.scene->settings.caustic.samplesPerEdge);
                const Vec3 sourcePoint = edgeA + (edgeB - edgeA) * t;
                const TextureSample sourceTexture = sampleTexture(source, sourcePoint);
                for (const Light& light : prepared.scene->lights) {
                    const Vec3 toLight = light.position - sourcePoint;
                    if (lengthSquared(toLight) <= 1.0e-12 || std::abs(toLight.y) <= 1.0e-12) {
                        continue;
                    }
                    const Vec3 toLightDirection = normalize(toLight);
                    const Vec3 tangent = normalize(cross(source.normal, toLightDirection));
                    const double shiftSign =
                        unitDouble(splitmix64(pieceSalt ^ light.id ^ 0x94d049bb133111ebULL)) < 0.5
                            ? -1.0
                            : 1.0;
                    const Vec3 thicknessShift =
                        tangent * (source.piece->thickness * 0.62 * shiftSign);
                    const double sampleWeight =
                        (edgeLength / static_cast<double>(prepared.scene->settings.caustic.samplesPerEdge)) /
                        std::max(source.characteristicSize, 1.0e-6);

                    const double outerT = (targetY - sourcePoint.y) / toLightDirection.y;
                    bool outerHit = false;
                    bool outerInside = false;
                    std::uint32_t outerPieceId = 0;
                    Vec3 outerPoint{};
                    if (outerT > 1.0e-8) {
                        const Vec3 candidate =
                            sourcePoint + toLightDirection * outerT + thicknessShift;
                        for (const std::size_t targetIndex : targetPiecesIt->second) {
                            const PreparedPiece& targetPiece = prepared.pieces[targetIndex];
                            for (const Triangle& triangle : targetPiece.piece->mesh.triangles) {
                                const Vec3& a =
                                    targetPiece.piece->mesh.vertices[triangle.vertices[0]];
                                const Vec3& b =
                                    targetPiece.piece->mesh.vertices[triangle.vertices[1]];
                                const Vec3& c =
                                    targetPiece.piece->mesh.vertices[triangle.vertices[2]];
                                if (pointInTriangle2D(candidate, a, b, c)) {
                                    outerHit = true;
                                    outerInside = true;
                                    outerPieceId = targetPiece.piece->pieceId;
                                    outerPoint = {candidate.x, targetY, candidate.z};
                                    break;
                                }
                            }
                            if (outerHit) {
                                break;
                            }
                        }
                        if (!outerHit) {
                            outerPoint = {candidate.x, targetY, candidate.z};
                        }
                    }

                    const Vec3 awayFromLight = normalize(sourcePoint - light.position);
                    const double floorT =
                        std::abs(awayFromLight.y) > 1.0e-12
                            ? (prepared.floorTop - sourcePoint.y) / awayFromLight.y
                            : -1.0;
                    if (floorT <= 1.0e-8) {
                        continue;
                    }
                    Vec3 floorPoint =
                        sourcePoint + awayFromLight * floorT +
                        tangent * (source.piece->thickness * 0.92 * shiftSign);
                    floorPoint.y = prepared.floorTop;

                    const double outerSigmaWorld =
                        prepared.scene->settings.caustic.penumbraBase +
                        prepared.scene->settings.caustic.penumbraThickness *
                            source.piece->thickness +
                        prepared.scene->settings.caustic.penumbraLayerDistance *
                            sourceToTargetDistance;
                    const double floorSigmaWorld =
                        prepared.scene->settings.caustic.penumbraBase +
                        prepared.scene->settings.caustic.penumbraThickness *
                            source.piece->thickness +
                        prepared.scene->settings.caustic.penumbraLayerDistance *
                            sourceToFloorDistance;
                    const double focalPixels =
                        static_cast<double>(height) /
                        (2.0 * prepared.camera.tanHalfFov);
                    const Rgb sourceTint =
                        sourceTexture.transmission * (0.55 + 0.45 * sourceTexture.bubble);
                    const Rgb color = clamp(light.color * sourceTint, 0.0, 1.0);
                    const double basePeak =
                        sampleWeight * light.intensity *
                        prepared.scene->settings.caustic.intensity * 3.2;

                    if (outerHit) {
                        CausticRecord record;
                        record.sampleIndex = sampleIndex++;
                        record.sourceLayer = sourceLayer;
                        record.sourcePieceId = source.piece->pieceId;
                        record.lightId = light.id;
                        record.sourcePoint = sourcePoint;
                        record.targetKind = CausticTargetKind::OuterPiece;
                        record.targetLayer = targetLayer;
                        record.targetPieceId = outerPieceId;
                        record.targetPoint = outerPoint;
                        record.thickness = source.piece->thickness;
                        record.layerDistance = sourceToTargetDistance;
                        record.penumbraSigmaWorld = outerSigmaWorld;
                        const auto projected = projectPoint(prepared.camera, outerPoint);
                        record.penumbraSigmaPixels = std::clamp(
                            outerSigmaWorld * focalPixels / std::max(projected.depth, 0.1),
                            0.35,
                            24.0);
                        record.screenX = projected.x;
                        record.screenY = projected.y;
                        record.peakBrightness = basePeak * 0.92;
                        record.outerGeometryHit = true;
                        result.records.push_back(record);
                        addCausticSplat(prepared, result, record, color);
                    }

                    CausticRecord floorRecord;
                    floorRecord.sampleIndex = sampleIndex++;
                    floorRecord.sourceLayer = sourceLayer;
                    floorRecord.sourcePieceId = source.piece->pieceId;
                    floorRecord.lightId = light.id;
                    floorRecord.sourcePoint = sourcePoint;
                    floorRecord.targetKind = CausticTargetKind::Floor;
                    floorRecord.targetLayer = -1;
                    floorRecord.targetPieceId = 0;
                    floorRecord.targetPoint = floorPoint;
                    floorRecord.thickness = source.piece->thickness;
                    floorRecord.layerDistance = sourceToFloorDistance;
                    floorRecord.penumbraSigmaWorld = floorSigmaWorld;
                    const auto projected = projectPoint(prepared.camera, floorPoint);
                    floorRecord.penumbraSigmaPixels = std::clamp(
                        floorSigmaWorld * focalPixels / std::max(projected.depth, 0.1),
                        0.35,
                        24.0);
                    floorRecord.screenX = projected.x;
                    floorRecord.screenY = projected.y;
                    floorRecord.peakBrightness = basePeak * (outerInside ? 0.58 : 1.24);
                    floorRecord.outerGeometryHit = outerInside;
                    result.records.push_back(floorRecord);
                    addCausticSplat(prepared, result, floorRecord, color);
                }
            }
        }
    }

    result.gray.resize(result.accum.size());
    for (std::size_t i = 0; i < result.accum.size(); ++i) {
        const double red = static_cast<double>(result.accum[i].r);
        const double green = static_cast<double>(result.accum[i].g);
        const double blue = static_cast<double>(result.accum[i].b);
        const double luminanceValue =
            (0.2126 * red + 0.7152 * green + 0.0722 * blue) / kCausticScale;
        result.gray[i] = static_cast<std::uint16_t>(
            std::min(65535.0, std::max(0.0, luminanceValue * 65535.0)));
    }
    result.stripeCount = countCausticStripes(result.gray, width, height);
    result.recordsBinary = encodeCausticRecords(prepared, result);
    result.grayBinary.reserve(result.gray.size() * 2);
    for (const std::uint16_t value : result.gray) {
        appendU16(result.grayBinary, value);
    }
    Sha256 hasher;
    hasher.update(result.recordsBinary);
    hasher.update(result.grayBinary);
    result.digest = Sha256::hex(hasher.finish());

    std::map<std::int32_t, const CausticRecord*> probeByLayer;
    for (const CausticRecord& record : result.records) {
        if (record.targetKind != CausticTargetKind::Floor) {
            continue;
        }
        const auto existing = probeByLayer.find(record.sourceLayer);
        if (existing == probeByLayer.end() ||
            std::tie(record.sourcePieceId, record.lightId) <
                std::tie(existing->second->sourcePieceId, existing->second->lightId)) {
            probeByLayer[record.sourceLayer] = &record;
        }
    }
    for (const auto& [layer, record] : probeByLayer) {
        PenumbraProbe probe;
        probe.sourceLayer = layer;
        probe.sourcePieceId = record->sourcePieceId;
        probe.lightId = record->lightId;
        probe.thickness = record->thickness;
        probe.layerDistance = record->layerDistance;
        probe.penumbraWidthPixels = record->penumbraSigmaPixels * 2.0;
        probe.peakBrightness = record->peakBrightness;
        const int centerX = static_cast<int>(std::lround(record->screenX));
        const int centerY = static_cast<int>(std::lround(record->screenY));
        const int offsets[4] = {
            0,
            static_cast<int>(std::lround(record->penumbraSigmaPixels * 0.5)),
            static_cast<int>(std::lround(record->penumbraSigmaPixels)),
            static_cast<int>(std::lround(record->penumbraSigmaPixels * 1.5)),
        };
        for (std::size_t i = 0; i < 4; ++i) {
            probe.profileGray[i] =
                grayAt(result, width, height, centerX + offsets[i], centerY);
        }
        result.probes.push_back(probe);
    }
    return result;
}

std::uint32_t chunkCount(const Scene& scene) {
    const std::uint32_t columns =
        (scene.settings.output.width + scene.settings.output.chunkWidth - 1U) /
        scene.settings.output.chunkWidth;
    const std::uint32_t rows =
        (scene.settings.output.height + scene.settings.output.chunkHeight - 1U) /
        scene.settings.output.chunkHeight;
    return columns * rows;
}

void chunkBounds(
    const Scene& scene,
    std::uint32_t chunkIndex,
    std::uint32_t& x,
    std::uint32_t& y,
    std::uint32_t& width,
    std::uint32_t& height) {
    const std::uint32_t columns =
        (scene.settings.output.width + scene.settings.output.chunkWidth - 1U) /
        scene.settings.output.chunkWidth;
    const std::uint32_t column = chunkIndex % columns;
    const std::uint32_t row = chunkIndex / columns;
    x = column * scene.settings.output.chunkWidth;
    y = row * scene.settings.output.chunkHeight;
    width = std::min(
        scene.settings.output.chunkWidth,
        scene.settings.output.width - x);
    height = std::min(
        scene.settings.output.chunkHeight,
        scene.settings.output.height - y);
}

ChunkResult renderChunk(
    const PreparedScene& prepared,
    const CausticResult& caustics,
    std::uint32_t chunkIndex) {
    ChunkResult result;
    const Scene& scene = *prepared.scene;
    result.index = chunkIndex;
    chunkBounds(scene, chunkIndex, result.x, result.y, result.width, result.height);
    result.rgba.resize(
        static_cast<std::size_t>(result.width) * result.height * 4U);
    result.ownership.resize(
        static_cast<std::size_t>(result.width) * result.height);
    result.layerStats.resize(prepared.layers.size());
    for (std::size_t i = 0; i < result.layerStats.size(); ++i) {
        result.layerStats[i].layerIndex = prepared.layers[i];
    }

    for (std::uint32_t row = 0; row < result.height; ++row) {
        for (std::uint32_t column = 0; column < result.width; ++column) {
            const std::uint32_t x = result.x + column;
            const std::uint32_t y = result.y + row;
            const PixelShade shade =
                shadePixel(prepared, caustics, x, y, result.layerStats);
            const std::size_t pixelIndex =
                static_cast<std::size_t>(row) * result.width + column;
            result.rgba[pixelIndex * 4] = toByte(shade.premultiplied.r);
            result.rgba[pixelIndex * 4 + 1] = toByte(shade.premultiplied.g);
            result.rgba[pixelIndex * 4 + 2] = toByte(shade.premultiplied.b);
            result.rgba[pixelIndex * 4 + 3] = toByte(shade.alpha);
            result.ownership[pixelIndex] = shade.owner;
        }
    }
    return result;
}

std::uint64_t ownershipKey(const Piece& piece) {
    return 0x8000000000000000ULL |
           (static_cast<std::uint64_t>(piece.layerIndex) << 32U) |
           static_cast<std::uint64_t>(piece.pieceId);
}

std::string causticRecordsJson(
    const PreparedScene& prepared,
    const CausticResult& caustics) {
    (void)prepared;
    std::ostringstream out;
    out << std::fixed << std::setprecision(9);
    out << "{\n  \"format\": \"mosaic-caustics\",\n  \"version\": 1,\n";
    out << "  \"recordCount\": " << caustics.records.size() << ",\n";
    out << "  \"stripeCount\": " << caustics.stripeCount << ",\n";
    out << "  \"digest\": \"" << caustics.digest << "\",\n";
    out << "  \"records\": [";
    for (std::size_t i = 0; i < caustics.records.size(); ++i) {
        const CausticRecord& record = caustics.records[i];
        out << (i == 0 ? "\n" : ",\n");
        out << "    {\"sampleIndex\": " << record.sampleIndex
            << ", \"sourceLayer\": " << record.sourceLayer
            << ", \"sourcePieceId\": " << record.sourcePieceId
            << ", \"lightId\": " << record.lightId
            << ", \"source\": [" << record.sourcePoint.x << ", " << record.sourcePoint.y
            << ", " << record.sourcePoint.z << "]"
            << ", \"targetKind\": \""
            << (record.targetKind == CausticTargetKind::OuterPiece ? "outerPiece" : "floor")
            << "\""
            << ", \"targetLayer\": " << record.targetLayer
            << ", \"targetPieceId\": " << record.targetPieceId
            << ", \"target\": [" << record.targetPoint.x << ", " << record.targetPoint.y
            << ", " << record.targetPoint.z << "]"
            << ", \"thickness\": " << record.thickness
            << ", \"layerDistance\": " << record.layerDistance
            << ", \"penumbraSigmaWorld\": " << record.penumbraSigmaWorld
            << ", \"penumbraSigmaPixels\": " << record.penumbraSigmaPixels
            << ", \"screen\": [" << record.screenX << ", " << record.screenY << "]"
            << ", \"peakBrightness\": " << record.peakBrightness
            << ", \"outerGeometryHit\": "
            << (record.outerGeometryHit ? "true" : "false") << "}";
    }
    if (!caustics.records.empty()) {
        out << '\n';
    }
    out << "  ]\n}\n";
    return out.str();
}

std::string ownershipLegendJson(const Scene& scene) {
    std::ostringstream out;
    out << "{\n  \"format\": \"mosaic-ownership\",\n  \"version\": 1,\n";
    out << "  \"encoding\": \"uint64-little-endian-row-major\",\n";
    out << "  \"floorKey\": 0,\n  \"noOwnerKey\": 18446744073709551615,\n";
    out << "  \"pieces\": [\n";
    for (std::size_t i = 0; i < scene.pieces.size(); ++i) {
        const Piece& piece = scene.pieces[i];
        out << "    {\"key\": " << ownershipKey(piece)
            << ", \"layerIndex\": " << piece.layerIndex
            << ", \"pieceId\": " << piece.pieceId << "}"
            << (i + 1 == scene.pieces.size() ? "\n" : ",\n");
    }
    out << "  ]\n}\n";
    return out.str();
}

std::string layerStatsJson(const std::vector<LayerStats>& stats) {
    std::ostringstream out;
    out << std::fixed << std::setprecision(6);
    out << "[";
    for (std::size_t i = 0; i < stats.size(); ++i) {
        const LayerStats& layer = stats[i];
        const double averageTransmission =
            layer.visibleFragments == 0
                ? 0.0
                : static_cast<double>(layer.transmissionLuminanceMicro) /
                      (static_cast<double>(layer.visibleFragments) * kStatsScale);
        const double transmissionContribution =
            static_cast<double>(layer.transmissionEnergyMicro) / kStatsScale;
        const double reflectionContribution =
            static_cast<double>(layer.reflectionEnergyMicro) / kStatsScale;
        const double totalContribution =
            transmissionContribution + reflectionContribution;
        const double transmissionRatio =
            totalContribution <= 0.0 ? 0.0 : transmissionContribution / totalContribution;
        const double reflectionRatio =
            totalContribution <= 0.0 ? 0.0 : reflectionContribution / totalContribution;
        out << (i == 0 ? "\n    " : ",\n    ");
        out << "{\"layerIndex\": " << layer.layerIndex
            << ", \"visibleFragments\": " << layer.visibleFragments
            << ", \"averageTransmission\": " << averageTransmission
            << ", \"transmissionContribution\": " << transmissionContribution
            << ", \"reflectionContribution\": " << reflectionContribution
            << ", \"transmissionRatio\": " << transmissionRatio
            << ", \"reflectionRatio\": " << reflectionRatio << "}";
    }
    if (!stats.empty()) {
        out << '\n';
    }
    out << "  ]";
    return out.str();
}

}  // namespace mosaic
