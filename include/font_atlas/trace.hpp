#pragma once

#include "font_atlas/json.hpp"
#include "font_atlas/types.hpp"

#include <mutex>
#include <string>
#include <vector>

namespace font_atlas {

class TraceLog {
public:
    void add(Json record);
    void addEvent(const std::string& event, const std::string& glyph,
                  int chunkIndex = -1);
    std::size_t size() const;
    void clear();
    const std::vector<Json>& records() const { return records_; }
    Result<void> writeJsonLines(const std::string& path) const;
    std::string toJsonLines() const;

private:
    mutable std::mutex mutex_;
    std::vector<Json> records_;
};

Result<void> writeByteIdenticalFile(const std::string& path, const std::string& content);

}  // namespace font_atlas

