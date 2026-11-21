#pragma once

#include <cstdint>
#include <limits>
#include <string>
#include <vector>

namespace chunkpipe {

enum class Code : std::uint32_t {
    Ok = 0,
    ChunkInvalid,
    VersionLoss,
    LoadFailed,
    VisibilityMismatch,
    VersionStale,
    RecoveryInvalid,
    Busy,
    Cancelled,
    IoError,
    Internal
};

const char* codeName(Code code);

struct ErrorLocation {
    std::uint32_t chunkId = std::numeric_limits<std::uint32_t>::max();
    std::uint32_t relatedChunkId = std::numeric_limits<std::uint32_t>::max();
    std::string field;
    std::string segment;
    std::string detail;
};

struct BusinessError {
    Code code = Code::Ok;
    std::string message;
    std::vector<ErrorLocation> locations;
};

template <typename T>
class Result {
public:
    Result() = default;

    static Result success(T value) {
        Result result;
        result.ok_ = true;
        result.value_ = std::move(value);
        return result;
    }

    static Result failure(BusinessError error) {
        Result result;
        result.ok_ = false;
        result.error_ = std::move(error);
        return result;
    }

    static Result failure(Code code, std::string message) {
        BusinessError error;
        error.code = code;
        error.message = std::move(message);
        return failure(std::move(error));
    }

    bool ok() const { return ok_; }
    explicit operator bool() const { return ok_; }

    T& value() { return value_; }
    const T& value() const { return value_; }
    T&& takeValue() { return std::move(value_); }

    BusinessError& error() { return error_; }
    const BusinessError& error() const { return error_; }

private:
    bool ok_ = false;
    T value_{};
    BusinessError error_{};
};

template <>
class Result<void> {
public:
    Result() : ok_(true) {}

    static Result success() {
        Result result;
        result.ok_ = true;
        return result;
    }

    static Result failure(BusinessError error) {
        Result result;
        result.ok_ = false;
        result.error_ = std::move(error);
        return result;
    }

    static Result failure(Code code, std::string message) {
        BusinessError error;
        error.code = code;
        error.message = std::move(message);
        return failure(std::move(error));
    }

    bool ok() const { return ok_; }
    explicit operator bool() const { return ok_; }

    BusinessError& error() { return error_; }
    const BusinessError& error() const { return error_; }

private:
    bool ok_ = false;
    BusinessError error_{};
};

} // namespace chunkpipe

