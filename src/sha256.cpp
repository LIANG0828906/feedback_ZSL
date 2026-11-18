#include "sha256.hpp"

#include <cstring>

namespace sr {
namespace {

constexpr std::array<uint32_t, 64> kRound = {
    0x428a2f98U, 0x71374491U, 0xb5c0fbcfU, 0xe9b5dba5U, 0x3956c25bU, 0x59f111f1U,
    0x923f82a4U, 0xab1c5ed5U, 0xd807aa98U, 0x12835b01U, 0x243185beU, 0x550c7dc3U,
    0x72be5d74U, 0x80deb1feU, 0x9bdc06a7U, 0xc19bf174U, 0xe49b69c1U, 0xefbe4786U,
    0x0fc19dc6U, 0x240ca1ccU, 0x2de92c6fU, 0x4a7484aaU, 0x5cb0a9dcU, 0x76f988daU,
    0x983e5152U, 0xa831c66dU, 0xb00327c8U, 0xbf597fc7U, 0xc6e00bf3U, 0xd5a79147U,
    0x06ca6351U, 0x14292967U, 0x27b70a85U, 0x2e1b2138U, 0x4d2c6dfcU, 0x53380d13U,
    0x650a7354U, 0x766a0abbU, 0x81c2c92eU, 0x92722c85U, 0xa2bfe8a1U, 0xa81a664bU,
    0xc24b8b70U, 0xc76c51a3U, 0xd192e819U, 0xd6990624U, 0xf40e3585U, 0x106aa070U,
    0x19a4c116U, 0x1e376c08U, 0x2748774cU, 0x34b0bcb5U, 0x391c0cb3U, 0x4ed8aa4aU,
    0x5b9cca4fU, 0x682e6ff3U, 0x748f82eeU, 0x78a5636fU, 0x84c87814U, 0x8cc70208U,
    0x90befffaU, 0xa4506cebU, 0xbef9a3f7U, 0xc67178f2U};

inline uint32_t rotateRight(uint32_t v, uint32_t n) { return (v >> n) | (v << (32U - n)); }

}  // namespace

Sha256::Sha256() {
    state_ = {0x6a09e667U, 0xbb67ae85U, 0x3c6ef372U, 0xa54ff53aU,
              0x510e527fU, 0x9b05688cU, 0x1f83d9abU, 0x5be0cd19U};
}

void Sha256::transform(const uint8_t* block) {
    uint32_t w[64]{};
    for (size_t i = 0; i < 16; ++i) {
        w[i] = (static_cast<uint32_t>(block[i * 4]) << 24U) |
               (static_cast<uint32_t>(block[i * 4 + 1]) << 16U) |
               (static_cast<uint32_t>(block[i * 4 + 2]) << 8U) |
               static_cast<uint32_t>(block[i * 4 + 3]);
    }
    for (size_t i = 16; i < 64; ++i) {
        const uint32_t s0 = rotateRight(w[i - 15], 7U) ^ rotateRight(w[i - 15], 18U) ^
                            (w[i - 15] >> 3U);
        const uint32_t s1 = rotateRight(w[i - 2], 17U) ^ rotateRight(w[i - 2], 19U) ^
                            (w[i - 2] >> 10U);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }

    uint32_t a = state_[0];
    uint32_t b = state_[1];
    uint32_t c = state_[2];
    uint32_t d = state_[3];
    uint32_t e = state_[4];
    uint32_t f = state_[5];
    uint32_t g = state_[6];
    uint32_t h = state_[7];

    for (size_t i = 0; i < 64; ++i) {
        const uint32_t s1 = rotateRight(e, 6U) ^ rotateRight(e, 11U) ^ rotateRight(e, 25U);
        const uint32_t ch = (e & f) ^ ((~e) & g);
        const uint32_t t1 = h + s1 + ch + kRound[i] + w[i];
        const uint32_t s0 = rotateRight(a, 2U) ^ rotateRight(a, 13U) ^ rotateRight(a, 22U);
        const uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        const uint32_t t2 = s0 + maj;
        h = g;
        g = f;
        f = e;
        e = d + t1;
        d = c;
        c = b;
        b = a;
        a = t1 + t2;
    }

    state_[0] += a;
    state_[1] += b;
    state_[2] += c;
    state_[3] += d;
    state_[4] += e;
    state_[5] += f;
    state_[6] += g;
    state_[7] += h;
}

void Sha256::update(const void* data, size_t size) {
    const auto* bytes = static_cast<const uint8_t*>(data);
    totalBytes_ += size;
    while (size > 0) {
        const size_t room = 64 - bufferSize_;
        const size_t take = size < room ? size : room;
        std::memcpy(buffer_.data() + bufferSize_, bytes, take);
        bufferSize_ += take;
        bytes += take;
        size -= take;
        if (bufferSize_ == 64) {
            transform(buffer_.data());
            bufferSize_ = 0;
        }
    }
}

void Sha256::update(const std::vector<uint8_t>& data) { update(data.data(), data.size()); }
void Sha256::update(const std::string& data) { update(data.data(), data.size()); }

std::array<uint8_t, 32> Sha256::finish() {
    const uint64_t bitLength = totalBytes_ * 8U;
    const uint8_t one = 0x80U;
    update(&one, 1);
    const uint8_t zero = 0;
    while (bufferSize_ != 56) {
        update(&zero, 1);
    }
    uint8_t lengthBytes[8]{};
    for (int i = 0; i < 8; ++i) {
        lengthBytes[7 - i] = static_cast<uint8_t>((bitLength >> (i * 8)) & 0xffU);
    }
    update(lengthBytes, 8);

    std::array<uint8_t, 32> result{};
    for (size_t i = 0; i < 8; ++i) {
        result[i * 4] = static_cast<uint8_t>((state_[i] >> 24U) & 0xffU);
        result[i * 4 + 1] = static_cast<uint8_t>((state_[i] >> 16U) & 0xffU);
        result[i * 4 + 2] = static_cast<uint8_t>((state_[i] >> 8U) & 0xffU);
        result[i * 4 + 3] = static_cast<uint8_t>(state_[i] & 0xffU);
    }
    return result;
}

std::array<uint8_t, 32> Sha256::hash(const void* data, size_t size) {
    Sha256 sha;
    sha.update(data, size);
    return sha.finish();
}

std::array<uint8_t, 32> Sha256::hash(const std::vector<uint8_t>& data) {
    return hash(data.data(), data.size());
}

std::array<uint8_t, 32> Sha256::hash(const std::string& data) {
    return hash(data.data(), data.size());
}

uint32_t crc32(const uint8_t* data, size_t size) {
    uint32_t crc = 0xffffffffU;
    for (size_t i = 0; i < size; ++i) {
        crc ^= data[i];
        for (int j = 0; j < 8; ++j) {
            const uint32_t mask = 0U - (crc & 1U);
            crc = (crc >> 1U) ^ (0xedb88320U & mask);
        }
    }
    return ~crc;
}

uint32_t adler32(const uint8_t* data, size_t size) {
    uint32_t a = 1;
    uint32_t b = 0;
    for (size_t i = 0; i < size; ++i) {
        a = (a + data[i]) % 65521U;
        b = (b + a) % 65521U;
    }
    return (b << 16U) | a;
}

}  // namespace sr

