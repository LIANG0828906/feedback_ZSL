#pragma once

#include <cstdint>
#include <string>

namespace preview {

enum class StatusCode : std::uint32_t {
    E_OK = 0,
    E_SCENE_INVALID,
    E_IO,
    E_BUSY,
    E_SNAPSHOT_STALE,
    E_OCCLUSION_UNCERTAIN,
    E_CANCELLED,
    E_SNAPSHOT_CORRUPT,
    E_INVALID_ARGUMENT,
    E_INTERNAL
};

const char* statusCodeName(StatusCode code) noexcept;

struct Diagnostic {
    StatusCode code = StatusCode::E_OK;
    std::uint32_t blockId = 0xffffffffu;
    std::string field;
    std::string message;
    std::uint64_t fileOffset = 0;

    bool ok() const noexcept { return code == StatusCode::E_OK; }
    explicit operator bool() const noexcept { return ok(); }
    std::string toJson() const;
};

Diagnostic makeDiagnostic(StatusCode code,
                          std::string field,
                          std::string message,
                          std::uint32_t blockId = 0xffffffffu,
                          std::uint64_t fileOffset = 0);

}  // namespace preview
