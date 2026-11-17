#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace mosaic {

class Sha256 {
public:
    Sha256();

    void update(const void* data, std::size_t size);
    void update(std::string_view text);
    void update(std::span<const std::uint8_t> bytes);
    std::array<std::uint8_t, 32> finish();

    static std::string hex(std::span<const std::uint8_t> digest);
    static std::string digest(std::span<const std::uint8_t> bytes);
    static std::string digest(std::string_view text);

private:
    void compress(const std::uint8_t block[64]);

    std::array<std::uint32_t, 8> state_;
    std::array<std::uint8_t, 64> buffer_{};
    std::size_t bufferSize_ = 0;
    std::uint64_t totalBytes_ = 0;
};

}  // namespace mosaic
