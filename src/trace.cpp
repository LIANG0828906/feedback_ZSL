#include "chunkpipe/trace.hpp"

#include <sstream>

namespace chunkpipe {

TraceLogger::TraceLogger(const std::filesystem::path& path) {
    const auto parent = path.parent_path();
    if (!parent.empty()) {
        std::error_code error;
        std::filesystem::create_directories(parent, error);
    }
    output_.open(path, std::ios::out | std::ios::trunc);
}

bool TraceLogger::isOpen() const {
    return output_.is_open();
}

void TraceLogger::event(
    const std::string& name,
    const std::vector<std::pair<std::string, std::string>>& fields) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (!output_) {
        return;
    }
    output_ << "{\"seq\":" << ++sequence_ << ",\"event\":\"" << jsonEscape(name)
            << "\"";
    for (const auto& field : fields) {
        output_ << ",\"" << jsonEscape(field.first) << "\":\""
                << jsonEscape(field.second) << "\"";
    }
    output_ << "}\n";
    output_.flush();
}

void TraceLogger::flush() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (output_) {
        output_.flush();
    }
}

std::string jsonEscape(const std::string& text) {
    std::ostringstream escaped;
    for (const char character : text) {
        switch (character) {
        case '"':
            escaped << "\\\"";
            break;
        case '\\':
            escaped << "\\\\";
            break;
        case '\n':
            escaped << "\\n";
            break;
        case '\r':
            escaped << "\\r";
            break;
        case '\t':
            escaped << "\\t";
            break;
        default:
            if (static_cast<unsigned char>(character) < 0x20) {
                escaped << "\\u00" << std::hex << static_cast<int>(
                    static_cast<unsigned char>(character));
            } else {
                escaped << character;
            }
        }
    }
    return escaped.str();
}

} // namespace chunkpipe

