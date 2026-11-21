#include "terrain/status.h"

namespace terrain {

const char* errorCodeName(ErrorCode code) {
    switch (code) {
        case ErrorCode::Ok:
            return "E_OK";
        case ErrorCode::HeightInvalid:
            return "E_HEIGHT_INVALID";
        case ErrorCode::PatchBounds:
            return "E_PATCH_BOUNDS";
        case ErrorCode::PatchStale:
            return "E_PATCH_STALE";
        case ErrorCode::CrackUnresolved:
            return "E_CRACK_UNRESOLVED";
        case ErrorCode::CommitFailed:
            return "E_COMMIT_FAILED";
        case ErrorCode::Busy:
            return "E_BUSY";
        case ErrorCode::Cancelled:
            return "E_CANCELLED";
        case ErrorCode::IoError:
            return "E_IO";
        case ErrorCode::InvalidArgument:
            return "E_INVALID_ARGUMENT";
        case ErrorCode::RecoveryFailed:
            return "E_RECOVERY_FAILED";
        case ErrorCode::RenderMismatch:
            return "E_RENDER_MISMATCH";
    }
    return "E_UNKNOWN";
}

Status Status::error(ErrorCode code, std::string field, std::string message) {
    Status out;
    out.code = code;
    out.field = std::move(field);
    out.message = std::move(message);
    return out;
}

Status Status::crack(std::string message, std::uint64_t block_a,
                     std::uint64_t block_b) {
    Status out = error(ErrorCode::CrackUnresolved, "seam", std::move(message));
    out.block_a = block_a;
    out.block_b = block_b;
    return out;
}

std::string Status::toString() const {
    if (ok()) {
        return "E_OK";
    }
    std::string out = errorCodeName(code);
    if (!field.empty()) {
        out += " field=";
        out += field;
    }
    if (!message.empty()) {
        out += " ";
        out += message;
    }
    if (block_a != 0 || block_b != 0) {
        out += " blocks=";
        out += std::to_string(block_a);
        out += ",";
        out += std::to_string(block_b);
    }
    return out;
}

}  // namespace terrain

