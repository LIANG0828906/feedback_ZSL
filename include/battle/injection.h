#pragma once

#include "battle/placement.h"
#include "battle/query.h"

#include <cstdint>

namespace battle {

enum class InjectionKind : std::uint8_t {
    None = 0,
    DegenerateUnitBounds = 1,
    ZeroLengthRay = 2,
    OutOfBoundsScreen = 3,
    SelfIntersectingDraft = 4,
    CorruptSnapshot = 5
};

struct InjectionSpec {
    InjectionKind kind = InjectionKind::None;
    std::uint32_t target_unit = 0;
};

// All injections are applied at the public input boundary. The query,
// placement, and version modules do not inspect InjectionKind.
class InjectionGate {
public:
    static Outcome<PickResult> pick_screen(const RegionStore& store,
                                           const SnapshotHandle& handle,
                                           const PickRequest& request,
                                           const InjectionSpec& injection);
    static Outcome<PickResult> pick_ray(const RegionStore& store,
                                        const SnapshotHandle& handle,
                                        Ray ray,
                                        const InjectionSpec& injection);
    static Outcome<PlacementResult> placement(
        const RegionStore& store, const SnapshotHandle& handle,
        const PlacementRequest& request, const InjectionSpec& injection);
};

const char* injection_kind_name(InjectionKind kind) noexcept;

}  // namespace battle

