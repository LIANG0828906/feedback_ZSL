#pragma once

#include "preview/error.hpp"
#include "preview/scene.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace preview {

struct SampleSceneConfig {
    int gridX = 4;
    int gridY = 3;
    int depthLayers = 3;
    double cellSize = 1.2;
    double layerSpacing = 1.5;
    double occluderDensity = 0.65;
    std::uint64_t seed = 0x5eed1234ull;
};

struct SampleSceneData {
    std::vector<BlockId> roots;
    std::vector<SceneBlock> blocks;
};

SampleSceneData generateSampleScene(const SampleSceneConfig& config);

struct SampleView {
    std::string name;
    Camera camera;
};

std::vector<SampleView> generateSampleViews(const SampleSceneConfig& sceneConfig,
                                            int viewCount,
                                            int width,
                                            int height);

bool writeViewSequenceFile(const std::string& path,
                           const std::vector<SampleView>& views,
                           Diagnostic* diagnostic = nullptr);
bool readViewSequenceFile(const std::string& path,
                          std::vector<SampleView>* views,
                          Diagnostic* diagnostic = nullptr);

}  // namespace preview
