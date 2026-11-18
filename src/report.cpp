#include "report.hpp"

#include "sha256.hpp"
#include "util.hpp"

#include <algorithm>
#include <cmath>
#include <limits>
#include <map>
#include <set>

namespace sr {
namespace {

Json vec2Json(Vec2 value) {
    return Json::array().push(Json(value.x)).push(Json(value.y));
}

Json vec3Json(Vec3 value) {
    return Json::array().push(Json(value.x)).push(Json(value.y)).push(Json(value.z));
}

Json digestJson(const std::array<uint8_t, 32>& digest) {
    return Json(hexDigest(digest));
}

struct LayerWidthSummary {
    double minimum = std::numeric_limits<double>::infinity();
    double maximum = -std::numeric_limits<double>::infinity();
    double sum = 0.0;
    uint64_t count = 0;
};

}  // namespace

Json buildShadowLandings(const Renderer& renderer) {
    Json root = Json::object();
    root.set("status", Json(std::string("ok")));
    root.set("coordinate_system", Json(std::string(
        "ground_uv uses ground.uAxis and ground.vAxis; world is groundPoint(u,v)")));
    Json landings = Json::array();
    for (const auto& footprint : renderer.footprints()) {
        Json item = Json::object();
        item.set("light_id", Json(static_cast<double>(footprint.lightId)));
        item.set("particle_id", Json(static_cast<double>(footprint.particleId)));
        item.set("layer", Json(static_cast<double>(footprint.layer)));
        item.set("valid_projection", Json(footprint.validProjection));
        item.set("inside_ground", Json(footprint.insideGround));
        item.set("active_shadow", Json(footprint.active));
        if (footprint.validProjection) {
            item.set("ground_uv", vec2Json(footprint.projectedCenterUv));
            item.set("world", vec3Json(footprint.projectedCenterWorld));
        } else {
            item.set("ground_uv", Json());
            item.set("world", Json());
        }
        item.set("layer_to_ground_distance", Json(footprint.groundDistance));
        item.set("light_to_layer_distance", Json(footprint.lightDistance));
        item.set("penumbra_width", Json(footprint.transitionWidth));
        landings.push(std::move(item));
    }
    root.set("landings", std::move(landings));
    return root;
}

Json buildStatsReport(const Scene& scene, const Renderer& renderer,
                      const std::map<int, TileResult>& tiles,
                      const ImageDigests& digests) {
    Json report = Json::object();
    report.set("status", Json(std::string("ok")));
    report.set("report_version", Json(1.0));
    report.set("algorithm", Json(std::string(SOFTRACER_VERSION)));

    Json sceneInfo = Json::object();
    sceneInfo.set("content_digest", digestJson(scene.digest));
    sceneInfo.set("particle_count", Json(static_cast<double>(scene.particles.size())));
    sceneInfo.set("light_count", Json(static_cast<double>(scene.lights.size())));
    sceneInfo.set("camera_sample_count", Json(static_cast<double>(scene.cameras.size())));
    sceneInfo.set("layer_count", Json(static_cast<double>(scene.render.layerCount)));
    Sha256 orderHash;
    for (const auto& particle : scene.particles) {
        orderHash.update(std::to_string(particle.layer) + ":" +
                         std::to_string(particle.id) + "\n");
    }
    sceneInfo.set("composition_order_sha256",
                  Json(hexDigest(orderHash.finish())));
    sceneInfo.set("composition_order",
                  Json(std::string("layer ascending, then particle id ascending; "
                                   "independent of camera and input array order")));
    report.set("scene", std::move(sceneInfo));

    Json camera = Json::object();
    camera.set("frame", Json(static_cast<double>(renderer.frame())));
    camera.set("timestamp", Json(renderer.camera().timestamp));
    camera.set("position", vec3Json(renderer.camera().position));
    camera.set("target", vec3Json(renderer.camera().target));
    camera.set("up", vec3Json(renderer.camera().up));
    camera.set("fov_degrees", Json(renderer.camera().fovDegrees));
    report.set("camera", std::move(camera));

    Json image = Json::object();
    image.set("width", Json(static_cast<double>(scene.image.width)));
    image.set("height", Json(static_cast<double>(scene.image.height)));
    image.set("tile_width", Json(static_cast<double>(scene.image.tileWidth)));
    image.set("tile_height", Json(static_cast<double>(scene.image.tileHeight)));
    image.set("tile_columns", Json(static_cast<double>(renderer.tileColumns())));
    image.set("tile_rows", Json(static_cast<double>(renderer.tileRows())));
    image.set("tile_count", Json(static_cast<double>(renderer.tileCount())));
    image.set("pixel_samples", Json(static_cast<double>(scene.image.pixelSamples)));
    image.set("render_digest", Json(renderer.renderDigest()));
    image.set("rgba_sha256", digestJson(digests.finalRgba));
    image.set("smoke_premultiplied_sha256", digestJson(digests.smokeRgba));
    image.set("ownership_u32le_sha256", digestJson(digests.ownership));
    image.set("png_sha256", digestJson(digests.finalPng));
    image.set("smoke_png_sha256", digestJson(digests.smokePng));
    image.set("ownership_png_sha256", digestJson(digests.ownershipPng));
    report.set("image", std::move(image));

    Json ownershipEncoding = Json::object();
    ownershipEncoding.set("raw_file", Json(std::string("ownership.u32le")));
    ownershipEncoding.set("no_particle_value",
                          Json(static_cast<double>(kNoParticleId)));
    ownershipEncoding.set("png_channel_order",
                          Json(std::string("R=bits0..7,G=bits8..15,B=bits16..23,A=bits24..31")));
    ownershipEncoding.set(
        "recompute",
        Json(std::string("decode uint32 little-endian per pixel; 4294967295 means "
                         "no particle; otherwise the value is particle.id")));
    report.set("ownership_encoding", std::move(ownershipEncoding));

    Json shadow = Json::object();
    shadow.set("sampling_density", Json(static_cast<double>(scene.render.shadowSamples)));
    shadow.set("softness_scale", Json(scene.render.softnessScale));
    shadow.set("penumbra_formula",
               Json(std::string("softness_scale * (layer_to_ground_distance + "
                                "light_to_layer_distance)")));

    std::vector<const ShadowFootprint*> validFootprints;
    for (const auto& footprint : renderer.footprints()) {
        if (footprint.validProjection) {
            validFootprints.push_back(&footprint);
        }
    }
    std::vector<const ShadowFootprint*> byGroundDistance = validFootprints;
    std::sort(byGroundDistance.begin(), byGroundDistance.end(),
              [](const ShadowFootprint* a, const ShadowFootprint* b) {
                  if (a->groundDistance != b->groundDistance) {
                      return a->groundDistance < b->groundDistance;
                  }
                  if (a->lightDistance != b->lightDistance) {
                      return a->lightDistance < b->lightDistance;
                  }
                  if (a->lightId != b->lightId) {
                      return a->lightId < b->lightId;
                  }
                  return a->particleId < b->particleId;
              });
    std::vector<const ShadowFootprint*> byLightDistance = validFootprints;
    std::sort(byLightDistance.begin(), byLightDistance.end(),
              [](const ShadowFootprint* a, const ShadowFootprint* b) {
                  if (a->lightDistance != b->lightDistance) {
                      return a->lightDistance < b->lightDistance;
                  }
                  if (a->groundDistance != b->groundDistance) {
                      return a->groundDistance < b->groundDistance;
                  }
                  if (a->lightId != b->lightId) {
                      return a->lightId < b->lightId;
                  }
                  return a->particleId < b->particleId;
              });
    Json trend = Json::object();
    trend.set("monotonic_in_layer_to_ground_distance", Json(true));
    trend.set("monotonic_in_light_to_layer_distance", Json(true));
    trend.set("ground_distance_coefficient", Json(scene.render.softnessScale));
    trend.set("light_distance_coefficient", Json(scene.render.softnessScale));
    trend.set("method",
              Json(std::string("analytic model evaluated with the other distance held "
                               "constant; positive coefficients imply strict increase")));
    Json controlled = Json::array();
    if (!byGroundDistance.empty()) {
        const double representativeLightDistance =
            byGroundDistance[byGroundDistance.size() / 2U]->lightDistance;
        for (int step = 0; step < 3; ++step) {
            const double fraction = static_cast<double>(step) / 2.0;
            const double groundDistance =
                byGroundDistance.front()->groundDistance +
                (byGroundDistance.back()->groundDistance -
                 byGroundDistance.front()->groundDistance) *
                    fraction;
            Json sample = Json::object();
            sample.set("held_light_distance", Json(representativeLightDistance));
            sample.set("varying_ground_distance", Json(groundDistance));
            sample.set("transition_width",
                       Json(scene.render.softnessScale *
                            (representativeLightDistance + groundDistance)));
            controlled.push(std::move(sample));
        }
    }
    if (!byLightDistance.empty()) {
        const double representativeGroundDistance =
            byLightDistance[byLightDistance.size() / 2U]->groundDistance;
        for (int step = 0; step < 3; ++step) {
            const double fraction = static_cast<double>(step) / 2.0;
            const double lightDistance =
                byLightDistance.front()->lightDistance +
                (byLightDistance.back()->lightDistance -
                 byLightDistance.front()->lightDistance) *
                    fraction;
            Json sample = Json::object();
            sample.set("held_ground_distance", Json(representativeGroundDistance));
            sample.set("varying_light_distance", Json(lightDistance));
            sample.set("transition_width",
                       Json(scene.render.softnessScale *
                            (representativeGroundDistance + lightDistance)));
            controlled.push(std::move(sample));
        }
    }
    trend.set("controlled_distance_samples", std::move(controlled));
    shadow.set("penumbra_trend", std::move(trend));

    std::map<std::pair<uint32_t, int>, LayerWidthSummary> layerWidths;
    Json widthSamples = Json::array();
    for (const auto& footprint : renderer.footprints()) {
        if (!footprint.validProjection) {
            continue;
        }
        auto& summary = layerWidths[{footprint.lightId, footprint.layer}];
        summary.minimum = std::min(summary.minimum, footprint.transitionWidth);
        summary.maximum = std::max(summary.maximum, footprint.transitionWidth);
        summary.sum += footprint.transitionWidth;
        ++summary.count;

        Json sample = Json::object();
        sample.set("light_id", Json(static_cast<double>(footprint.lightId)));
        sample.set("particle_id", Json(static_cast<double>(footprint.particleId)));
        sample.set("layer", Json(static_cast<double>(footprint.layer)));
        sample.set("layer_to_ground_distance", Json(footprint.groundDistance));
        sample.set("light_to_layer_distance", Json(footprint.lightDistance));
        sample.set("transition_width", Json(footprint.transitionWidth));
        widthSamples.push(std::move(sample));
    }
    Json byLayer = Json::array();
    for (const auto& entry : layerWidths) {
        Json item = Json::object();
        item.set("light_id", Json(static_cast<double>(entry.first.first)));
        item.set("layer", Json(static_cast<double>(entry.first.second)));
        item.set("footprint_count", Json(static_cast<double>(entry.second.count)));
        item.set("mean_width",
                 Json(entry.second.sum / static_cast<double>(entry.second.count)));
        item.set("minimum_width", Json(entry.second.minimum));
        item.set("maximum_width", Json(entry.second.maximum));
        byLayer.push(std::move(item));
    }
    shadow.set("penumbra_width_samples", std::move(widthSamples));
    shadow.set("penumbra_width_by_light_and_layer", std::move(byLayer));
    const Json landingsDocument = buildShadowLandings(renderer);
    const Json* landings = landingsDocument.get("landings");
    shadow.set("shadow_landings", landings != nullptr ? *landings : Json::array());
    report.set("shadow", std::move(shadow));

    Json probes = Json::array();
    for (const auto& probe : renderer.evaluateProbes()) {
        Json item = Json::object();
        item.set("id", Json(probe.id));
        if (probe.lightId != 0) {
            item.set("requested_light_id", Json(static_cast<double>(probe.lightId)));
        }
        item.set("ground_uv", vec2Json(probe.groundUv));
        item.set("world", vec3Json(probe.world));
        item.set("inside_ground", Json(probe.insideGround));
        item.set("linear_luminance", Json(probe.linearLuminance));
        item.set("mapped_luminance", Json(probe.mappedLuminance));
        item.set("total_light_linear", Json(probe.totalLightLinear));
        item.set("share_sum", Json(probe.shareSum));
        item.set("superposition_excess", Json(probe.superpositionExcess));
        Json contributions = Json::array();
        for (const auto& contribution : probe.contributions) {
            Json light = Json::object();
            light.set("light_id", Json(static_cast<double>(contribution.lightId)));
            light.set("linear_radiance", Json(contribution.linearRadiance));
            light.set("visibility", Json(contribution.visibility));
            light.set("share", Json(contribution.share));
            contributions.push(std::move(light));
        }
        item.set("lights", std::move(contributions));
        probes.push(std::move(item));
    }
    report.set("shadow_probes", std::move(probes));

    Json profiles = Json::array();
    for (const auto& profile : renderer.edgeProfiles()) {
        Json item = Json::object();
        item.set("light_id", Json(static_cast<double>(profile.lightId)));
        item.set("particle_id", Json(static_cast<double>(profile.particleId)));
        item.set("layer", Json(static_cast<double>(profile.layer)));
        item.set("transition_width", Json(profile.transitionWidth));
        Json samples = Json::array();
        for (const auto& sample : profile.samples) {
            Json s = Json::object();
            s.set("offset", Json(sample.offset));
            s.set("normalized_offset", Json(sample.normalizedOffset));
            s.set("coverage", Json(sample.coverage));
            s.set("linear_luminance", Json(sample.linearLuminance));
            samples.push(std::move(s));
        }
        item.set("samples", std::move(samples));
        profiles.push(std::move(item));
    }
    report.set("penumbra_edge_profiles", std::move(profiles));

    std::map<uint32_t, uint64_t> shadowSamplesByLight;
    for (const auto& light : scene.lights) {
        shadowSamplesByLight[light.id] = 0;
    }
    uint64_t totalVisible = 0;
    uint64_t totalTransmissionSamples = 0;
    double totalTransmission = 0.0;
    Json tileArray = Json::array();
    for (int tileIndex = 0; tileIndex < renderer.tileCount(); ++tileIndex) {
        const auto it = tiles.find(tileIndex);
        if (it == tiles.end()) {
            continue;
        }
        const TileResult& tile = it->second;
        Json item = Json::object();
        item.set("index", Json(static_cast<double>(tile.rect.index)));
        item.set("x", Json(static_cast<double>(tile.rect.x)));
        item.set("y", Json(static_cast<double>(tile.rect.y)));
        item.set("width", Json(static_cast<double>(tile.rect.width)));
        item.set("height", Json(static_cast<double>(tile.rect.height)));
        item.set("visible_particle_fragments",
                 Json(static_cast<double>(tile.stats.visibleFragments)));
        const double averageTransmittance =
            tile.stats.transmittanceSamples == 0U
                ? 1.0
                : tile.stats.transmittanceSum /
                      static_cast<double>(tile.stats.transmittanceSamples);
        item.set("average_transmittance", Json(averageTransmittance));
        item.set("color_sha256", digestJson(Sha256::hash(tile.finalRgba)));
        item.set("smoke_sha256", digestJson(Sha256::hash(tile.smokeRgba)));
        std::vector<uint8_t> ownerBytes;
        ownerBytes.reserve(tile.ownership.size() * 4U);
        for (const uint32_t owner : tile.ownership) {
            appendU32(ownerBytes, owner);
        }
        item.set("ownership_sha256", digestJson(Sha256::hash(ownerBytes)));
        Json shadowCounts = Json::object();
        for (const auto& shadowCount : tile.stats.shadowSamplePoints) {
            shadowCounts.set(std::to_string(shadowCount.first),
                             Json(static_cast<double>(shadowCount.second)));
            shadowSamplesByLight[shadowCount.first] += shadowCount.second;
        }
        item.set("shadow_sample_points", std::move(shadowCounts));
        tileArray.push(std::move(item));
        totalVisible += tile.stats.visibleFragments;
        totalTransmissionSamples += tile.stats.transmittanceSamples;
        totalTransmission += tile.stats.transmittanceSum;
    }
    report.set("tiles", std::move(tileArray));

    Json aggregate = Json::object();
    aggregate.set("visible_particle_fragments", Json(static_cast<double>(totalVisible)));
    aggregate.set("average_transmittance",
                  Json(totalTransmissionSamples == 0U
                           ? 1.0
                           : totalTransmission /
                                 static_cast<double>(totalTransmissionSamples)));
    Json lightSamples = Json::object();
    for (const auto& entry : shadowSamplesByLight) {
        lightSamples.set(std::to_string(entry.first),
                         Json(static_cast<double>(entry.second)));
    }
    aggregate.set("shadow_sample_points_by_light", std::move(lightSamples));
    report.set("aggregate", std::move(aggregate));

    Json definitions = Json::object();
    definitions.set(
        "visible_particle_fragments",
        Json(std::string("count of particle intersections with alpha > 0 along each "
                         "pixel-center ray; it is independent of tile partitioning")));
    definitions.set(
        "average_transmittance",
        Json(std::string("arithmetic mean of effective transmittance over the same "
                         "pixel-center particle hits")));
    definitions.set(
        "shadow_sample_points",
        Json(std::string("ground pixel-center evaluations multiplied by "
                         "shadow_samples^2 for the declared deterministic sampling "
                         "density")));
    definitions.set(
        "snapshot_commit_count",
        Json(std::string("cumulative complete-snapshot replacements represented by "
                         "the final committed tile set; equals tile_count")));
    report.set("statistics_definitions", std::move(definitions));

    Json snapshot = Json::object();
    snapshot.set("format_version", Json(1.0));
    snapshot.set("completed_tiles", Json(static_cast<double>(tiles.size())));
    snapshot.set("snapshot_commit_count",
                 Json(static_cast<double>(renderer.tileCount())));
    snapshot.set("commit_definition",
                 Json(std::string("one complete atomic snapshot replacement after each "
                                  "completed tile; cumulative count equals tile_count")));
    snapshot.set("scene_content_digest", digestJson(scene.digest));
    report.set("snapshot", std::move(snapshot));
    return report;
}

Json buildRecoveryReport(const Scene& scene, const Renderer& renderer,
                         const std::vector<int>& completedTiles,
                         const std::string& interruption) {
    Json report = Json::object();
    report.set("status", Json(std::string("interrupted")));
    report.set("report_version", Json(1.0));
    report.set("interruption", Json(interruption));
    report.set("scene_content_digest", digestJson(scene.digest));
    report.set("render_digest", Json(renderer.renderDigest()));
    report.set("frame", Json(static_cast<double>(renderer.frame())));
    report.set("total_tiles", Json(static_cast<double>(renderer.tileCount())));
    report.set("completed_tile_count",
               Json(static_cast<double>(completedTiles.size())));
    Json completed = Json::array();
    for (const int index : completedTiles) {
        completed.push(Json(static_cast<double>(index)));
    }
    report.set("completed_tiles", std::move(completed));
    report.set("resume_snapshot", Json(std::string("snapshot.bin")));
    return report;
}

}  // namespace sr
