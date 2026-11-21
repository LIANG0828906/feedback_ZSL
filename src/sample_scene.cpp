#include "preview/sample_scene.hpp"

#include <algorithm>
#include <cmath>
#include <fstream>
#include <iomanip>
#include <locale>
#include <sstream>

namespace preview {

namespace {

std::uint64_t splitMix64(std::uint64_t* state) {
    std::uint64_t value = (*state += 0x9e3779b97f4a7c15ull);
    value = (value ^ (value >> 30u)) * 0xbf58476d1ce4e5b9ull;
    value = (value ^ (value >> 27u)) * 0x94d049bb133111ebull;
    return value ^ (value >> 31u);
}

double unitDouble(std::uint64_t* state) {
    return static_cast<double>(splitMix64(state) >> 11u) *
           (1.0 / 9007199254740992.0);
}

void appendCube(const Bounds3& bounds,
                std::vector<SceneVertex>* vertices,
                std::vector<std::uint32_t>* indices) {
    const auto corners = bounds.corners();
    for (const Vec3& corner : corners) {
        vertices->push_back(SceneVertex{static_cast<float>(corner.x),
                                        static_cast<float>(corner.y),
                                        static_cast<float>(corner.z)});
    }
    static const std::uint32_t cubeIndices[36] = {
        0, 2, 1, 1, 2, 3,
        4, 5, 6, 5, 7, 6,
        0, 1, 4, 1, 5, 4,
        2, 6, 3, 3, 6, 7,
        0, 4, 2, 2, 4, 6,
        1, 3, 5, 3, 7, 5
    };
    for (std::uint32_t index : cubeIndices) {
        indices->push_back(index);
    }
}

}  // namespace

SampleSceneData generateSampleScene(const SampleSceneConfig& config) {
    SampleSceneData scene;
    const int gridX = std::max(1, config.gridX);
    const int gridY = std::max(1, config.gridY);
    const int layers = std::max(1, config.depthLayers);
    const double cellSize = std::max(0.01, config.cellSize);
    const double layerSpacing = std::max(0.01, config.layerSpacing);

    const BlockId rootId = 1;
    std::vector<BlockId> layerIds;
    layerIds.reserve(static_cast<std::size_t>(layers));
    for (int layer = 0; layer < layers; ++layer) {
        layerIds.push_back(static_cast<BlockId>(2 + layer));
    }
    BlockId nextLeafId = static_cast<BlockId>(2 + layers);

    std::uint64_t randomState = config.seed;
    std::vector<SceneBlock> allBlocks;
    Bounds3 rootBounds;

    for (int layer = 0; layer < layers; ++layer) {
        SceneBlock group;
        group.id = layerIds[static_cast<std::size_t>(layer)];
        group.parent = rootId;
        group.priority = static_cast<std::uint32_t>((layers - layer) * 100);
        group.occluder = false;
        Bounds3 groupBounds;
        const double z = -static_cast<double>(layer) * layerSpacing;
        for (int y = 0; y < gridY; ++y) {
            for (int x = 0; x < gridX; ++x) {
                SceneBlock leaf;
                leaf.id = nextLeafId++;
                leaf.parent = group.id;
                const double centerX =
                    (static_cast<double>(x) -
                     static_cast<double>(gridX - 1) * 0.5) *
                    cellSize;
                const double centerY =
                    (static_cast<double>(y) -
                     static_cast<double>(gridY - 1) * 0.5) *
                    cellSize;
                const double half =
                    cellSize * (0.52 + 0.03 * unitDouble(&randomState));
                leaf.bounds.min = {centerX - half, centerY - half,
                                   z - layerSpacing * 0.30};
                leaf.bounds.max = {centerX + half, centerY + half,
                                   z + layerSpacing * 0.30};
                const bool frontLayer = layer == 0;
                leaf.occluder =
                    frontLayer || unitDouble(&randomState) < config.occluderDensity;
                leaf.priority = static_cast<std::uint32_t>(
                    (layers - layer) * 100 + (gridX - x) + (gridY - y));
                appendCube(leaf.bounds, &leaf.vertices, &leaf.indices);
                group.children.push_back(leaf.id);
                groupBounds.expand(leaf.bounds);
                allBlocks.push_back(std::move(leaf));
            }
        }
        group.bounds = groupBounds;
        rootBounds.expand(groupBounds);
        allBlocks.push_back(std::move(group));
    }

    SceneBlock root;
    root.id = rootId;
    root.parent = kInvalidBlockId;
    root.bounds = rootBounds;
    root.priority = 1000;
    root.occluder = false;
    root.children = layerIds;
    allBlocks.push_back(std::move(root));

    std::sort(allBlocks.begin(), allBlocks.end(),
              [](const SceneBlock& a, const SceneBlock& b) {
                  return a.id < b.id;
              });
    scene.roots.push_back(rootId);
    scene.blocks = std::move(allBlocks);
    return scene;
}

std::vector<SampleView> generateSampleViews(const SampleSceneConfig& sceneConfig,
                                            int viewCount,
                                            int width,
                                            int height) {
    const int count = std::max(1, viewCount);
    const int layers = std::max(1, sceneConfig.depthLayers);
    const double spacing = std::max(0.01, sceneConfig.layerSpacing);
    const double gridSpan =
        std::max(sceneConfig.gridX, sceneConfig.gridY) *
        std::max(0.01, sceneConfig.cellSize);
    const double targetZ = -static_cast<double>(layers - 1) * spacing * 0.5;
    const double baseDistance = std::max(5.0, gridSpan * 1.6 + 2.0);
    const double orbitRadius = std::max(0.5, gridSpan * 0.25);

    std::vector<SampleView> views;
    views.reserve(static_cast<std::size_t>(count));
    for (int i = 0; i < count; ++i) {
        const double phase = 2.0 * 3.14159265358979323846 *
                             static_cast<double>(i) /
                             static_cast<double>(count);
        const double dolly =
            static_cast<double>((i * 3) % 5) * 0.18;
        SampleView view;
        std::ostringstream name;
        name << "view_" << std::setfill('0') << std::setw(3) << i;
        view.name = name.str();
        view.camera.position = {std::cos(phase) * orbitRadius * 0.35,
                                std::sin(phase) * orbitRadius * 0.25,
                                targetZ + baseDistance + dolly};
        view.camera.target = {0.0, 0.0, targetZ};
        view.camera.up = {0.0, 1.0, 0.0};
        view.camera.fovYDegrees = 60.0;
        view.camera.nearPlane = 0.1;
        view.camera.farPlane =
            std::max(100.0, baseDistance + static_cast<double>(layers) * spacing +
                                20.0);
        view.camera.width = width;
        view.camera.height = height;
        views.push_back(std::move(view));
    }
    return views;
}

bool writeViewSequenceFile(const std::string& path,
                           const std::vector<SampleView>& views,
                           Diagnostic* diagnostic) {
    std::ofstream stream(path, std::ios::trunc);
    if (!stream) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_IO, "views.path",
                                         "failed to create view sequence file");
        }
        return false;
    }
    stream.imbue(std::locale::classic());
    stream << std::setprecision(17);
    stream << "ZSPVVIEW1\n" << views.size() << "\n";
    for (const SampleView& view : views) {
        stream << view.name << ' ' << view.camera.position.x << ' '
               << view.camera.position.y << ' ' << view.camera.position.z << ' '
               << view.camera.target.x << ' ' << view.camera.target.y << ' '
               << view.camera.target.z << ' ' << view.camera.up.x << ' '
               << view.camera.up.y << ' ' << view.camera.up.z << ' '
               << view.camera.fovYDegrees << ' ' << view.camera.nearPlane << ' '
               << view.camera.farPlane << ' ' << view.camera.width << ' '
               << view.camera.height << '\n';
    }
    stream.flush();
    if (!stream) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_IO, "views.path",
                                         "failed to write view sequence file");
        }
        return false;
    }
    if (diagnostic != nullptr) {
        *diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    }
    return true;
}

bool readViewSequenceFile(const std::string& path,
                          std::vector<SampleView>* views,
                          Diagnostic* diagnostic) {
    if (views == nullptr) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_INVALID_ARGUMENT,
                                         "views.output", "output is null");
        }
        return false;
    }
    std::ifstream stream(path);
    if (!stream) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_IO, "views.path",
                                         "failed to open view sequence file");
        }
        return false;
    }
    stream.imbue(std::locale::classic());
    std::string magic;
    std::uint64_t count = 0;
    if (!(stream >> magic >> count) || magic != "ZSPVVIEW1" ||
        count > 1'000'000u) {
        if (diagnostic != nullptr) {
            *diagnostic = makeDiagnostic(StatusCode::E_SCENE_INVALID,
                                         "views.header",
                                         "invalid view sequence header");
        }
        return false;
    }
    views->clear();
    views->reserve(static_cast<std::size_t>(count));
    for (std::uint64_t i = 0; i < count; ++i) {
        SampleView view;
        if (!(stream >> view.name >> view.camera.position.x >>
              view.camera.position.y >> view.camera.position.z >>
              view.camera.target.x >> view.camera.target.y >>
              view.camera.target.z >> view.camera.up.x >> view.camera.up.y >>
              view.camera.up.z >> view.camera.fovYDegrees >>
              view.camera.nearPlane >> view.camera.farPlane >>
              view.camera.width >> view.camera.height)) {
            if (diagnostic != nullptr) {
                *diagnostic = makeDiagnostic(
                    StatusCode::E_SCENE_INVALID, "views.entry",
                    "truncated view entry");
            }
            return false;
        }
        if (view.camera.width <= 0 || view.camera.height <= 0 ||
            !(view.camera.nearPlane > 0.0) ||
            !(view.camera.farPlane > view.camera.nearPlane)) {
            if (diagnostic != nullptr) {
                *diagnostic = makeDiagnostic(
                    StatusCode::E_SCENE_INVALID, "views.entry",
                    "invalid view camera parameters");
            }
            return false;
        }
        views->push_back(std::move(view));
    }
    if (diagnostic != nullptr) {
        *diagnostic = makeDiagnostic(StatusCode::E_OK, "", "");
    }
    return true;
}

}  // namespace preview
