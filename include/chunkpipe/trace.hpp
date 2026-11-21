#pragma once

#include <filesystem>
#include <fstream>
#include <mutex>
#include <string>
#include <utility>
#include <vector>

namespace chunkpipe {

class TraceLogger {
public:
    explicit TraceLogger(const std::filesystem::path& path);

    bool isOpen() const;
    void event(const std::string& name,
               const std::vector<std::pair<std::string, std::string>>& fields = {});
    void flush();

private:
    std::mutex mutex_;
    std::ofstream output_;
    std::uint64_t sequence_ = 0;
};

std::string jsonEscape(const std::string& text);

} // namespace chunkpipe

