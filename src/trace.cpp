#include "font_atlas/trace.hpp"

#include "font_atlas/io.hpp"

#include <sstream>

namespace font_atlas {

void TraceLog::add(Json record) {
    std::lock_guard<std::mutex> lock(mutex_);
    records_.push_back(std::move(record));
}

void TraceLog::addEvent(const std::string& event, const std::string& glyph, int chunkIndex) {
    Json record = Json::object();
    record["event"] = Json(event);
    record["glyph"] = Json(glyph);
    if (chunkIndex >= 0) {
        record["chunk"] = Json(chunkIndex);
    }
    add(std::move(record));
}

std::size_t TraceLog::size() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return records_.size();
}

void TraceLog::clear() {
    std::lock_guard<std::mutex> lock(mutex_);
    records_.clear();
}

std::string TraceLog::toJsonLines() const {
    std::lock_guard<std::mutex> lock(mutex_);
    std::ostringstream stream;
    for (const auto& record : records_) {
        stream << record.dump() << '\n';
    }
    return stream.str();
}

Result<void> TraceLog::writeJsonLines(const std::string& path) const {
    return writeByteIdenticalFile(path, toJsonLines());
}

Result<void> writeByteIdenticalFile(const std::string& path, const std::string& content) {
    return writeFileAtomic(path, content);
}

}  // namespace font_atlas
