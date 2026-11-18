#pragma once

#include "render.h"
#include "scene.h"

#include <string>
#include <vector>

namespace cr {

struct SnapshotLoadResult {
    bool present = false;
    bool valid = false;
    bool discarded = false;
    std::string reason;
    std::vector<TileData> tiles;
};

bool saveSnapshot(const std::string& path, const Scene& scene,
                  const std::vector<TileData>& tiles, std::string& error);

SnapshotLoadResult loadSnapshot(const std::string& path, const Scene& scene);

void discardSnapshot(const std::string& path);

json::Value snapshotManifest(const Scene& scene, const std::vector<TileData>& tiles);

}  // namespace cr

