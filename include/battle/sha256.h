#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>

namespace battle {

using Digest = std::array<std::uint8_t, 32>;

Digest sha256(const void* data, std::size_t size);
Digest sha256(const std::string& data);
std::string digest_hex(const Digest& digest);
bool parse_digest_hex(const std::string& text, Digest& out);

class Sha256Builder {
public:
    void append(const void* data, std::size_t size);
    void append(const std::string& text);
    void append_u8(std::uint8_t value);
    void append_u32(std::uint32_t value);
    void append_u64(std::uint64_t value);
    void append_f32_bits(float value);
    Digest finish() const;

private:
    std::string bytes_;
};

}  // namespace battle

