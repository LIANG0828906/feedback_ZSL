#pragma once

#include <cstdint>
#include <string>

namespace terrain {

enum class ErrorCode : int {
    Ok = 0,
    HeightInvalid,
    PatchBounds,
    PatchStale,
    CrackUnresolved,
    CommitFailed,
    Busy,
    Cancelled,
    IoError,
    InvalidArgument,
    RecoveryFailed,
    RenderMismatch
};

inline constexpr ErrorCode E_OK = ErrorCode::Ok;
inline constexpr ErrorCode E_HEIGHT_INVALID = ErrorCode::HeightInvalid;
inline constexpr ErrorCode E_PATCH_BOUNDS = ErrorCode::PatchBounds;
inline constexpr ErrorCode E_PATCH_STALE = ErrorCode::PatchStale;
inline constexpr ErrorCode E_CRACK_UNRESOLVED = ErrorCode::CrackUnresolved;
inline constexpr ErrorCode E_COMMIT_FAILED = ErrorCode::CommitFailed;
inline constexpr ErrorCode E_BUSY = ErrorCode::Busy;
inline constexpr ErrorCode E_CANCELLED = ErrorCode::Cancelled;
inline constexpr ErrorCode E_IO = ErrorCode::IoError;
inline constexpr ErrorCode E_INVALID_ARGUMENT = ErrorCode::InvalidArgument;
inline constexpr ErrorCode E_RECOVERY_FAILED = ErrorCode::RecoveryFailed;
inline constexpr ErrorCode E_RENDER_MISMATCH = ErrorCode::RenderMismatch;

const char* errorCodeName(ErrorCode code);

struct Status {
    ErrorCode code = ErrorCode::Ok;
    std::string field;
    std::string message;
    std::uint64_t block_a = 0;
    std::uint64_t block_b = 0;

    static Status success() { return {}; }
    static Status error(ErrorCode code, std::string field, std::string message);
    static Status crack(std::string message, std::uint64_t block_a, std::uint64_t block_b);

    bool ok() const { return code == ErrorCode::Ok; }
    explicit operator bool() const { return ok(); }
    std::string toString() const;
};

}  // namespace terrain

