#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace cr {

using Digest = std::array<std::uint8_t, 32>;

Digest sha256(const std::uint8_t* data, std::size_t size);
Digest sha256(std::string_view text);
std::string hexDigest(const Digest& digest);
std::string sha256Hex(const std::uint8_t* data, std::size_t size);
std::string sha256Hex(std::string_view text);

std::string base64Encode(const std::vector<std::uint8_t>& data);
bool base64Decode(const std::string& text, std::vector<std::uint8_t>& data);

std::uint32_t crc32(const std::uint8_t* data, std::size_t size);

}  // namespace cr

