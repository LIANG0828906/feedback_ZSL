#include "core/Error.h"

namespace meshedit {

const char* codeName(Code code) {
    switch (code) {
    case Code::Ok: return "E_OK";
    case Code::ParseInvalid: return "E_PARSE_INVALID";
    case Code::WeldInvalid: return "E_WELD_INVALID";
    case Code::VersionLoss: return "E_VERSION_LOSS";
    case Code::Busy: return "E_BUSY";
    case Code::ProjectMismatch: return "E_PROJECT_MISMATCH";
    case Code::FormatLoss: return "E_FORMAT_LOSS";
    case Code::IoError: return "E_IO";
    case Code::InvalidArgument: return "E_INVALID_ARGUMENT";
    case Code::Cancelled: return "E_CANCELLED";
    case Code::Internal: return "E_INTERNAL";
    }
    return "E_INTERNAL";
}

} // namespace meshedit
