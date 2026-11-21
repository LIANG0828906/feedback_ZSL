#pragma once

#include "preview/error.hpp"
#include "preview/scene.hpp"

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace preview {

constexpr std::uint32_t kSceneFormatVersion = 1;
constexpr std::size_t kSceneHeaderSize = 40;

struct SceneLoadResult {
    std::shared_ptr<const Scene> scene;
    Diagnostic diagnostic;

    bool ok() const noexcept { return scene != nullptr && diagnostic.ok(); }
    explicit operator bool() const noexcept { return ok(); }
};

SceneLoadResult parseSceneBinary(const std::vector<std::uint8_t>& bytes);
SceneLoadResult loadSceneFile(const std::string& path);

bool encodeSceneBinary(const std::vector<BlockId>& roots,
                       const std::vector<SceneBlock>& blocks,
                       std::uint32_t flags,
                       std::vector<std::uint8_t>* output,
                       Diagnostic* diagnostic = nullptr);

bool writeSceneFile(const std::string& path,
                    const std::vector<BlockId>& roots,
                    const std::vector<SceneBlock>& blocks,
                    std::uint32_t flags = 0,
                    Diagnostic* diagnostic = nullptr);

}  // namespace preview
