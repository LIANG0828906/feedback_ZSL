#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace sr {

class Sha256 {
public:
    Sha256();
    void update(const void* data, size_t size);
    void update(const std::vector<uint8_t>& data);
    void update(const std::string& data);
    std::array<uint8_t, 32> finish();

    static std::array<uint8_t, 32> hash(const void* data, size_t size);
    static std::array<uint8_t, 32> hash(const std::vector<uint8_t>& data);
    static std::array<uint8_t, 32> hash(const std::string& data);

private:
    void transform(const uint8_t* block);

    std::array<uint32_t, 8> state_{};
    std::array<uint8_t, 64> buffer_{};
    uint64_t totalBytes_ = 0;
    size_t bufferSize_ = 0;
};

uint32_t crc32(const uint8_t* data, size_t size);
uint32_t adler32(const uint8_t* data, size_t size);

}  // namespace sr

