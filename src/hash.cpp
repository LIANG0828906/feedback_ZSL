#include "preview/hash.hpp"

#include <iomanip>
#include <sstream>

namespace preview {

std::uint64_t fnv1a64(const void* data, std::size_t size, std::uint64_t seed) noexcept {
    const auto* bytes = static_cast<const unsigned char*>(data);
    std::uint64_t value = seed;
    for (std::size_t i = 0; i < size; ++i) {
        value ^= static_cast<std::uint64_t>(bytes[i]);
        value *= kFnvPrime;
    }
    return value;
}

std::uint64_t hashCombine(std::uint64_t seed, std::uint64_t value) noexcept {
    seed ^= value + 0x9e3779b97f4a7c15ull + (seed << 6u) + (seed >> 2u);
    return seed;
}

std::string hex64(std::uint64_t value) {
    std::ostringstream out;
    out << std::hex << std::setfill('0') << std::setw(16) << value;
    return out.str();
}

void HashBuilder::addBytes(const void* data, std::size_t size) noexcept {
    value_ = fnv1a64(data, size, value_);
}

void HashBuilder::addString(const std::string& value) noexcept {
    const std::uint64_t size = static_cast<std::uint64_t>(value.size());
    addPod(size);
    addBytes(value.data(), value.size());
}

}  // namespace preview
