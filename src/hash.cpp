#include "shadowpipe/hash.hpp"

#include <cstring>
#include <iomanip>
#include <locale>
#include <sstream>

#include "shadowpipe/math.hpp"

namespace shadowpipe {

void Hasher::bytes(const void* data, std::size_t size) {
    const auto* p = static_cast<const std::uint8_t*>(data);
    for (std::size_t i = 0; i < size; ++i) {
        hash_ ^= static_cast<std::uint64_t>(p[i]);
        hash_ *= 1099511628211ULL;
    }
}

void Hasher::u32(std::uint32_t value) {
    const std::uint8_t bytes_le[4] = {
        static_cast<std::uint8_t>(value & 0xffu),
        static_cast<std::uint8_t>((value >> 8u) & 0xffu),
        static_cast<std::uint8_t>((value >> 16u) & 0xffu),
        static_cast<std::uint8_t>((value >> 24u) & 0xffu),
    };
    bytes(bytes_le, sizeof(bytes_le));
}

void Hasher::u64(std::uint64_t value) {
    u32(static_cast<std::uint32_t>(value & 0xffffffffULL));
    u32(static_cast<std::uint32_t>(value >> 32u));
}

void Hasher::f32(float value) {
    std::uint32_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "float must be 32 bit");
    std::memcpy(&bits, &value, sizeof(bits));
    u32(bits);
}

void Hasher::string(const std::string& value) {
    u64(static_cast<std::uint64_t>(value.size()));
    bytes(value.data(), value.size());
}

std::uint64_t hashBytes(const void* data, std::size_t size) {
    Hasher hasher;
    hasher.bytes(data, size);
    return hasher.value();
}

std::string hashHex(std::uint64_t hash) {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << std::hex << std::setfill('0') << std::setw(16) << hash;
    return out.str();
}

std::string formatFloat(float value, int precision) {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << std::fixed << std::setprecision(precision) << safeFloat(value);
    return out.str();
}

}  // namespace shadowpipe

