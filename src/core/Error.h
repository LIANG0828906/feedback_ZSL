#pragma once

#include <optional>
#include <string>
#include <utility>
#include <vector>

namespace meshedit {

enum class Code {
    Ok = 0,
    ParseInvalid,
    WeldInvalid,
    VersionLoss,
    Busy,
    ProjectMismatch,
    FormatLoss,
    IoError,
    InvalidArgument,
    Cancelled,
    Internal
};

const char* codeName(Code code);

struct Error {
    Code code = Code::Ok;
    std::string message;
    int line = 0;
    std::vector<std::string> affectedGroups;

    bool ok() const { return code == Code::Ok; }
    explicit operator bool() const { return ok(); }
};

template <typename T>
struct Result {
    std::optional<T> value;
    Error error;

    Result() = default;
    Result(T v) : value(std::move(v)) {}
    Result(Error e) : error(std::move(e)) {}

    bool ok() const { return value.has_value() && error.ok(); }
    explicit operator bool() const { return ok(); }

    T& get() { return *value; }
    const T& get() const { return *value; }
    T* operator->() { return &*value; }
    const T* operator->() const { return &*value; }
};

inline Result<bool> success() { return Result<bool>(true); }
inline Result<bool> failure(Code code, std::string message, int line = 0) {
    Error e;
    e.code = code;
    e.message = std::move(message);
    e.line = line;
    return Result<bool>(std::move(e));
}

} // namespace meshedit
