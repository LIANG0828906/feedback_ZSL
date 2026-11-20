#pragma once

#include <cstdint>
#include <string>

namespace shadowpipe {

enum class ErrorCode : std::int32_t {
    Ok = 0,
    SceneInvalid = 1,
    OcclusionAmbiguous = 2,
    BiasInvalid = 3,
    SnapshotMismatch = 4,
    Busy = 5,
    Cancelled = 6,
    IoError = 7,
    InvalidArgument = 8,
    InternalError = 9,
};

const char* errorCodeName(ErrorCode code);

struct Status {
    ErrorCode code = ErrorCode::Ok;
    std::string message;
    std::int64_t node_index = -1;
    std::string node_name;

    bool ok() const noexcept { return code == ErrorCode::Ok; }

    static Status success() { return {}; }

    static Status error(ErrorCode code, std::string message) {
        Status status;
        status.code = code;
        status.message = std::move(message);
        return status;
    }

    static Status errorAt(ErrorCode code, std::string message, std::int64_t nodeIndex,
                          std::string nodeName = {}) {
        Status status;
        status.code = code;
        status.message = std::move(message);
        status.node_index = nodeIndex;
        status.node_name = std::move(nodeName);
        return status;
    }
};

}  // namespace shadowpipe

