#include "font_atlas/types.hpp"

namespace font_atlas {

const char* errorCodeName(ErrorCode code) {
    switch (code) {
        case ErrorCode::Ok:
            return "OK";
        case ErrorCode::FontInvalid:
            return "E_FONT_INVALID";
        case ErrorCode::QuantInvalid:
            return "E_QUANT_INVALID";
        case ErrorCode::Busy:
            return "E_BUSY";
        case ErrorCode::BudgetExceeded:
            return "E_BUDGET_EXCEEDED";
        case ErrorCode::SeamUnresolved:
            return "E_SEAM_UNRESOLVED";
        case ErrorCode::AtlasOverflow:
            return "E_ATLAS_OVERFLOW";
        case ErrorCode::SnapshotMismatch:
            return "E_SNAPSHOT_MISMATCH";
        case ErrorCode::Cancelled:
            return "E_CANCELLED";
        case ErrorCode::IoError:
            return "E_IO";
        case ErrorCode::InvalidArgument:
            return "E_ARGUMENT";
        case ErrorCode::Internal:
            return "E_INTERNAL";
    }
    return "E_UNKNOWN";
}

const char* fillRuleName(FillRule rule) {
    return rule == FillRule::NonZero ? "nonzero" : "evenodd";
}

bool parseFillRule(const std::string& text, FillRule& rule) {
    if (text == "nonzero" || text == "non-zero" || text == "NonZero") {
        rule = FillRule::NonZero;
        return true;
    }
    if (text == "evenodd" || text == "even-odd" || text == "EvenOdd") {
        rule = FillRule::EvenOdd;
        return true;
    }
    return false;
}

}  // namespace font_atlas
