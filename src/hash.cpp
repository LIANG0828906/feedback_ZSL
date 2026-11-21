#include "terrain/hash.h"

#include <cstring>

namespace terrain {
namespace {

constexpr std::uint32_t kRound[64] = {
    0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu,
    0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u, 0xd807aa98u, 0x12835b01u,
    0x243185beu, 0x550c7dc3u, 0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u,
    0xc19bf174u, 0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu,
    0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau, 0x983e5152u,
    0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u,
    0x06ca6351u, 0x14292967u, 0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu,
    0x53380d13u, 0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
    0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u, 0xd192e819u,
    0xd6990624u, 0xf40e3585u, 0x106aa070u, 0x19a4c116u, 0x1e376c08u,
    0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu,
    0x682e6ff3u, 0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u,
    0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};

inline std::uint32_t rotr(std::uint32_t x, std::uint32_t n) {
    return (x >> n) | (x << (32u - n));
}

Digest finalize(const std::uint8_t* data, std::size_t size) {
    std::uint32_t h[8] = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
                          0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};

    const std::uint64_t bit_length = static_cast<std::uint64_t>(size) * 8u;
    const std::size_t padded_size = ((size + 9u + 63u) / 64u) * 64u;
    std::vector<std::uint8_t> padded(padded_size, 0);
    if (size != 0) {
        std::memcpy(padded.data(), data, size);
    }
    padded[size] = 0x80u;
    for (int i = 0; i < 8; ++i) {
        padded[padded_size - 1u - static_cast<std::size_t>(i)] =
            static_cast<std::uint8_t>((bit_length >> (8u * i)) & 0xffu);
    }

    for (std::size_t chunk = 0; chunk < padded_size; chunk += 64) {
        std::uint32_t w[64];
        for (int i = 0; i < 16; ++i) {
            const std::size_t p = chunk + static_cast<std::size_t>(i) * 4u;
            w[i] = (static_cast<std::uint32_t>(padded[p]) << 24u) |
                   (static_cast<std::uint32_t>(padded[p + 1]) << 16u) |
                   (static_cast<std::uint32_t>(padded[p + 2]) << 8u) |
                   static_cast<std::uint32_t>(padded[p + 3]);
        }
        for (int i = 16; i < 64; ++i) {
            const std::uint32_t s0 =
                rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
            const std::uint32_t s1 =
                rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16] + s0 + w[i - 7] + s1;
        }

        std::uint32_t a = h[0];
        std::uint32_t b = h[1];
        std::uint32_t c = h[2];
        std::uint32_t d = h[3];
        std::uint32_t e = h[4];
        std::uint32_t f = h[5];
        std::uint32_t g = h[6];
        std::uint32_t hh = h[7];
        for (int i = 0; i < 64; ++i) {
            const std::uint32_t s1 =
                rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
            const std::uint32_t ch = (e & f) ^ ((~e) & g);
            const std::uint32_t temp1 =
                hh + s1 + ch + kRound[i] + w[i];
            const std::uint32_t s0 =
                rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
            const std::uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
            const std::uint32_t temp2 = s0 + maj;
            hh = g;
            g = f;
            f = e;
            e = d + temp1;
            d = c;
            c = b;
            b = a;
            a = temp1 + temp2;
        }
        h[0] += a;
        h[1] += b;
        h[2] += c;
        h[3] += d;
        h[4] += e;
        h[5] += f;
        h[6] += g;
        h[7] += hh;
    }

    Digest out{};
    for (int i = 0; i < 8; ++i) {
        out[static_cast<std::size_t>(i) * 4u] =
            static_cast<std::uint8_t>((h[i] >> 24u) & 0xffu);
        out[static_cast<std::size_t>(i) * 4u + 1u] =
            static_cast<std::uint8_t>((h[i] >> 16u) & 0xffu);
        out[static_cast<std::size_t>(i) * 4u + 2u] =
            static_cast<std::uint8_t>((h[i] >> 8u) & 0xffu);
        out[static_cast<std::size_t>(i) * 4u + 3u] =
            static_cast<std::uint8_t>(h[i] & 0xffu);
    }
    return out;
}

}  // namespace

Digest sha256(const void* data, std::size_t size) {
    return finalize(static_cast<const std::uint8_t*>(data), size);
}

Digest sha256(const std::vector<std::uint8_t>& bytes) {
    return finalize(bytes.data(), bytes.size());
}

std::string toHex(const Digest& digest) {
    static constexpr char kHex[] = "0123456789abcdef";
    std::string out;
    out.resize(64);
    for (std::size_t i = 0; i < digest.size(); ++i) {
        out[i * 2u] = kHex[digest[i] >> 4u];
        out[i * 2u + 1u] = kHex[digest[i] & 0x0fu];
    }
    return out;
}

std::string toHex(const void* data, std::size_t size) {
    return toHex(sha256(data, size));
}

Digest fromHexOrZero(const std::string& text) {
    Digest out{};
    if (text.size() != 64) {
        return out;
    }
    auto nibble = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    };
    for (std::size_t i = 0; i < 32; ++i) {
        const int hi = nibble(text[i * 2u]);
        const int lo = nibble(text[i * 2u + 1u]);
        if (hi < 0 || lo < 0) {
            return zeroDigest();
        }
        out[i] = static_cast<std::uint8_t>((hi << 4) | lo);
    }
    return out;
}

Digest hashBytesWithPrefix(const Digest& prefix, const void* data,
                           std::size_t size) {
    std::vector<std::uint8_t> combined;
    combined.reserve(prefix.size() + size);
    combined.insert(combined.end(), prefix.begin(), prefix.end());
    const auto* bytes = static_cast<const std::uint8_t*>(data);
    combined.insert(combined.end(), bytes, bytes + size);
    return sha256(combined);
}

Digest hashFloats(const std::vector<float>& values) {
    std::vector<std::uint8_t> raw;
    raw.reserve(values.size() * sizeof(float));
    for (float value : values) {
        std::uint32_t bits = 0;
        static_assert(sizeof(bits) == sizeof(value), "float32 required");
        std::memcpy(&bits, &value, sizeof(bits));
        raw.push_back(static_cast<std::uint8_t>(bits & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((bits >> 8u) & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((bits >> 16u) & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((bits >> 24u) & 0xffu));
    }
    return sha256(raw);
}

Digest hashU32(const std::vector<std::uint32_t>& values) {
    std::vector<std::uint8_t> raw;
    raw.reserve(values.size() * sizeof(std::uint32_t));
    for (std::uint32_t value : values) {
        raw.push_back(static_cast<std::uint8_t>(value & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xffu));
        raw.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xffu));
    }
    return sha256(raw);
}

Digest zeroDigest() {
    Digest out{};
    out.fill(0);
    return out;
}

}  // namespace terrain

