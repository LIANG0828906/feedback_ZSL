#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace shadowpipe {

class Hasher {
public:
    Hasher() = default;

    void bytes(const void* data, std::size_t size);
    void u8(std::uint8_t value) { bytes(&value, sizeof(value)); }
    void u32(std::uint32_t value);
    void i32(std::int32_t value) { u32(static_cast<std::uint32_t>(value)); }
    void u64(std::uint64_t value);
    void f32(float value);
    void string(const std::string& value);

    std::uint64_t value() const noexcept { return hash_; }

private:
    std::uint64_t hash_ = 1469598103934665603ULL;
};

std::uint64_t hashBytes(const void* data, std::size_t size);
std::string hashHex(std::uint64_t hash);

}  // namespace shadowpipe

