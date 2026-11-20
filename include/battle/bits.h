#pragma once

#include <cstdint>
#include <cstring>
#include <iomanip>
#include <sstream>
#include <string>

namespace battle {

inline std::uint32_t float_bits(float value) noexcept {
    std::uint32_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "float must be 32-bit");
    std::memcpy(&bits, &value, sizeof(bits));
    return bits;
}

inline float bits_float(std::uint32_t bits) noexcept {
    float value = 0.0F;
    std::memcpy(&value, &bits, sizeof(value));
    return value;
}

inline std::string hex32(std::uint32_t value) {
    std::ostringstream stream;
    stream << std::hex << std::setfill('0') << std::setw(8) << value;
    return stream.str();
}

inline std::string float_hex(float value) {
    return hex32(float_bits(value));
}

inline bool parse_hex32(const std::string& text, std::uint32_t& value) {
    if (text.size() != 8) {
        return false;
    }
    std::uint32_t result = 0;
    for (char c : text) {
        unsigned digit = 0;
        if (c >= '0' && c <= '9') {
            digit = static_cast<unsigned>(c - '0');
        } else if (c >= 'a' && c <= 'f') {
            digit = static_cast<unsigned>(c - 'a' + 10);
        } else if (c >= 'A' && c <= 'F') {
            digit = static_cast<unsigned>(c - 'A' + 10);
        } else {
            return false;
        }
        result = (result << 4U) | digit;
    }
    value = result;
    return true;
}

}  // namespace battle

