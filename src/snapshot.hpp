#pragma once

#include "render.hpp"

#include <map>
#include <string>
#include <vector>

namespace sl {

struct SnapshotBlockRecord {
    int id = 0;
    int width = 0;
    int height = 0;
    std::vector<std::uint8_t> rgba;
    std::vector<std::uint8_t> owner;
    std::vector<LayerMetric> metrics;
    std::string rgbaDigest;
    std::string ownerDigest;
};

struct SnapshotData {
    std::string sceneDigest;
    std::string sceneSummaryJson;
    int imageWidth = 0;
    int imageHeight = 0;
    int blockWidth = 0;
    int blockHeight = 0;
    std::map<int, SnapshotBlockRecord> completed;
    std::string documentDigest;
};

struct SnapshotLoadResult {
    bool exists = false;
    bool valid = false;
    std::string reason;
    std::string documentDigest;
    SnapshotData data;
};

class SnapshotStore {
public:
    explicit SnapshotStore(std::string path) : path_(std::move(path)) {}

    const std::string& path() const { return path_; }
    SnapshotLoadResult load(const Renderer& renderer) const;
    std::string commit(const Renderer& renderer,
                       const std::map<int, SnapshotBlockRecord>& completed) const;
    void discard() const;

private:
    std::string path_;
};

std::string base64Encode(const std::vector<std::uint8_t>& bytes);
bool base64Decode(const std::string& text, std::vector<std::uint8_t>& bytes);
void writeFileAtomic(const std::string& path, const std::string& contents);
bool readFile(const std::string& path, std::string& contents);

} // namespace sl

