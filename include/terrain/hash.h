#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace terrain {

using Digest = std::array<std::uint8_t, 32>;

Digest sha256(const void* data, std::size_t size);
Digest sha256(const std::vector<std::uint8_t>& bytes);
std::string toHex(const Digest& digest);
std::string toHex(const void* data, std::size_t size);
Digest fromHexOrZero(const std::string& text);
Digest hashBytesWithPrefix(const Digest& prefix, const void* data,
                           std::size_t size);
Digest hashFloats(const std::vector<float>& values);
Digest hashU32(const std::vector<std::uint32_t>& values);
Digest zeroDigest();

}  // namespace terrain

