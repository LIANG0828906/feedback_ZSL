#include "preview/error.hpp"

#include <sstream>

namespace preview {

const char* statusCodeName(StatusCode code) noexcept {
    switch (code) {
        case StatusCode::E_OK: return "E_OK";
        case StatusCode::E_SCENE_INVALID: return "E_SCENE_INVALID";
        case StatusCode::E_IO: return "E_IO";
        case StatusCode::E_BUSY: return "E_BUSY";
        case StatusCode::E_SNAPSHOT_STALE: return "E_SNAPSHOT_STALE";
        case StatusCode::E_OCCLUSION_UNCERTAIN: return "E_OCCLUSION_UNCERTAIN";
        case StatusCode::E_CANCELLED: return "E_CANCELLED";
        case StatusCode::E_SNAPSHOT_CORRUPT: return "E_SNAPSHOT_CORRUPT";
        case StatusCode::E_INVALID_ARGUMENT: return "E_INVALID_ARGUMENT";
        case StatusCode::E_INTERNAL: return "E_INTERNAL";
    }
    return "E_INTERNAL";
}

Diagnostic makeDiagnostic(StatusCode code,
                          std::string field,
                          std::string message,
                          std::uint32_t blockId,
                          std::uint64_t fileOffset) {
    Diagnostic result;
    result.code = code;
    result.blockId = blockId;
    result.field = std::move(field);
    result.message = std::move(message);
    result.fileOffset = fileOffset;
    return result;
}

namespace {

std::string escapeJson(const std::string& value) {
    std::string result;
    result.reserve(value.size() + 8);
    for (char ch : value) {
        switch (ch) {
            case '"': result += "\\\""; break;
            case '\\': result += "\\\\"; break;
            case '\n': result += "\\n"; break;
            case '\r': result += "\\r"; break;
            case '\t': result += "\\t"; break;
            default: result += ch; break;
        }
    }
    return result;
}

}  // namespace

std::string Diagnostic::toJson() const {
    std::ostringstream out;
    out << "{\"code\":\"" << statusCodeName(code) << "\""
        << ",\"block_id\":";
    if (blockId == 0xffffffffu) {
        out << "null";
    } else {
        out << blockId;
    }
    out << ",\"field\":\"" << escapeJson(field) << "\""
        << ",\"message\":\"" << escapeJson(message) << "\""
        << ",\"file_offset\":" << fileOffset << "}";
    return out.str();
}

}  // namespace preview
