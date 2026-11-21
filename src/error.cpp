#include "chunkpipe/error.hpp"

namespace chunkpipe {

const char* codeName(Code code) {
    switch (code) {
    case Code::Ok:
        return "E_OK";
    case Code::ChunkInvalid:
        return "E_CHUNK_INVALID";
    case Code::VersionLoss:
        return "E_VERSION_LOSS";
    case Code::LoadFailed:
        return "E_LOAD_FAILED";
    case Code::VisibilityMismatch:
        return "E_VISIBILITY_MISMATCH";
    case Code::VersionStale:
        return "E_VERSION_STALE";
    case Code::RecoveryInvalid:
        return "E_RECOVERY_INVALID";
    case Code::Busy:
        return "E_BUSY";
    case Code::Cancelled:
        return "E_CANCELLED";
    case Code::IoError:
        return "E_IO_ERROR";
    case Code::Internal:
        return "E_INTERNAL";
    }
    return "E_INTERNAL";
}

} // namespace chunkpipe

