#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace font_atlas {

class Sha256 {
public:
    Sha256();

    void update(const void* data, std::size_t length);
    void update(const std::string& text);
    std::string finalHex();

    static std::string hex(const void* data, std::size_t length);
    static std::string hex(const std::string& text);

private:
    void transform(const std::uint8_t block[64]);

    std::uint32_t state_[8];
    std::uint64_t bitCount_;
    std::uint8_t buffer_[64];
    std::size_t bufferSize_;
    bool finalized_;
};

}  // namespace font_atlas

