#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace caisson {

class Sha256 {
public:
    Sha256();

    void update(const void* data, std::size_t size);
    void update(const std::string& text);
    void update(const std::vector<std::uint8_t>& bytes);
    std::array<std::uint8_t, 32> finish();

    static std::array<std::uint8_t, 32> digest(const void* data, std::size_t size);
    static std::array<std::uint8_t, 32> digest(const std::string& text);
    static std::array<std::uint8_t, 32> digest(const std::vector<std::uint8_t>& bytes);

private:
    void transform(const std::uint8_t* block);

    std::array<std::uint32_t, 8> state_;
    std::array<std::uint8_t, 64> buffer_{};
    std::uint64_t bitCount_ = 0;
    std::size_t bufferSize_ = 0;
    bool finished_ = false;
};

std::string toHex(const std::array<std::uint8_t, 32>& digest);
std::string sha256Hex(const std::string& text);
std::string sha256Hex(const std::vector<std::uint8_t>& bytes);

}  // namespace caisson

