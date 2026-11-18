#pragma once

#include "renderer.hpp"

#include <array>
#include <cstdint>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

namespace sr {

enum class SnapshotLoadState {
    Missing,
    Loaded,
    DiscardedCorrupt,
    DiscardedMismatch
};

struct SnapshotLoadResult {
    SnapshotLoadState state = SnapshotLoadState::Missing;
    std::string detail;
    std::map<int, TileResult> tiles;
};

class SnapshotStore {
public:
    explicit SnapshotStore(std::filesystem::path path) : path_(std::move(path)) {}

    SnapshotLoadResult load(const Scene& scene, int frame, const Renderer& renderer) const;
    bool commit(const Scene& scene, int frame, const Renderer& renderer,
                const std::map<int, TileResult>& tiles, std::string& error) const;
    const std::filesystem::path& path() const { return path_; }

private:
    std::filesystem::path path_;
};

}  // namespace sr

