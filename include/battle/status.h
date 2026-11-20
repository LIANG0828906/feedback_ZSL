#pragma once

#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace battle {

enum class StatusCode : std::uint32_t {
    Ok = 0,
    SceneInvalid = 1001,
    RayInvalid = 1002,
    RegionStale = 1003,
    RegionInvalid = 1004,
    SnapshotStale = 1005,
    TraceInvalid = 1006,
    PlacementRejected = 1007,
    IoError = 1008,
    UsageError = 1009
};

inline const char* status_name(StatusCode code) noexcept {
    switch (code) {
        case StatusCode::Ok: return "OK";
        case StatusCode::SceneInvalid: return "E_SCENE_INVALID";
        case StatusCode::RayInvalid: return "E_RAY_INVALID";
        case StatusCode::RegionStale: return "E_REGION_STALE";
        case StatusCode::RegionInvalid: return "E_REGION_INVALID";
        case StatusCode::SnapshotStale: return "E_SNAPSHOT_STALE";
        case StatusCode::TraceInvalid: return "E_TRACE_INVALID";
        case StatusCode::PlacementRejected: return "E_PLACEMENT_REJECTED";
        case StatusCode::IoError: return "E_IO";
        case StatusCode::UsageError: return "E_USAGE";
    }
    return "E_UNKNOWN";
}

enum class EntityKind : std::uint8_t {
    None = 0,
    Unit = 1,
    Cover = 2,
    Ground = 3
};

inline const char* entity_kind_name(EntityKind kind) noexcept {
    switch (kind) {
        case EntityKind::None: return "none";
        case EntityKind::Unit: return "unit";
        case EntityKind::Cover: return "cover";
        case EntityKind::Ground: return "ground";
    }
    return "unknown";
}

struct EntityRef {
    EntityKind kind = EntityKind::None;
    std::uint32_t id = 0;

    bool operator==(const EntityRef& other) const noexcept {
        return kind == other.kind && id == other.id;
    }

    bool operator!=(const EntityRef& other) const noexcept {
        return !(*this == other);
    }

    bool operator<(const EntityRef& other) const noexcept {
        if (kind != other.kind) {
            return static_cast<std::uint8_t>(kind) < static_cast<std::uint8_t>(other.kind);
        }
        return id < other.id;
    }
};

inline std::string entity_ref_string(const EntityRef& entity) {
    if (entity.kind == EntityKind::None) {
        return "none";
    }
    return std::string(entity_kind_name(entity.kind)) + ":" + std::to_string(entity.id);
}

struct Error {
    StatusCode code = StatusCode::Ok;
    std::string message;
    std::string injection_point;
    EntityRef entity;
    int vertex_index = -1;
    int edge_index = -1;
    std::vector<EntityRef> conflicts;
};

template <typename T>
class Outcome {
public:
    Outcome() = default;

    static Outcome success(T value) {
        Outcome result;
        result.ok_ = true;
        result.value_ = std::move(value);
        return result;
    }

    static Outcome failure(StatusCode code, std::string message,
                           std::string injection_point = {}) {
        Outcome result;
        result.error_.code = code;
        result.error_.message = std::move(message);
        result.error_.injection_point = std::move(injection_point);
        return result;
    }

    static Outcome failure(Error error) {
        Outcome result;
        result.error_ = std::move(error);
        return result;
    }

    bool ok() const noexcept { return ok_; }
    explicit operator bool() const noexcept { return ok_; }

    T& value() { return value_; }
    const T& value() const { return value_; }

    Error& error() { return error_; }
    const Error& error() const { return error_; }

private:
    bool ok_ = false;
    T value_{};
    Error error_{};
};

template <>
class Outcome<void> {
public:
    Outcome() = default;

    static Outcome success() {
        Outcome result;
        result.ok_ = true;
        return result;
    }

    static Outcome failure(StatusCode code, std::string message,
                           std::string injection_point = {}) {
        Outcome result;
        result.error_.code = code;
        result.error_.message = std::move(message);
        result.error_.injection_point = std::move(injection_point);
        return result;
    }

    static Outcome failure(Error error) {
        Outcome result;
        result.error_ = std::move(error);
        return result;
    }

    bool ok() const noexcept { return ok_; }
    explicit operator bool() const noexcept { return ok_; }

    Error& error() { return error_; }
    const Error& error() const { return error_; }

private:
    bool ok_ = false;
    Error error_{};
};

}  // namespace battle

