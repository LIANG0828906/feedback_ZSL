#include "battle/sha256.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <iomanip>
#include <sstream>

namespace battle {
namespace {

constexpr std::array<std::uint32_t, 8> kInitial = {
    0x6a09e667U, 0xbb67ae85U, 0x3c6ef372U, 0xa54ff53aU,
    0x510e527fU, 0x9b05688cU, 0x1f83d9abU, 0x5be0cd19U
};

constexpr std::array<std::uint32_t, 64> kRound = {
    0x428a2f98U, 0x71374491U, 0xb5c0fbcfU, 0xe9b5dba5U,
    0x3956c25bU, 0x59f111f1U, 0x923f82a4U, 0xab1c5ed5U,
    0xd807aa98U, 0x12835b01U, 0x243185beU, 0x550c7dc3U,
    0x72be5d74U, 0x80deb1feU, 0x9bdc06a7U, 0xc19bf174U,
    0xe49b69c1U, 0xefbe4786U, 0x0fc19dc6U, 0x240ca1ccU,
    0x2de92c6fU, 0x4a7484aaU, 0x5cb0a9dcU, 0x76f988daU,
    0x983e5152U, 0xa831c66dU, 0xb00327c8U, 0xbf597fc7U,
    0xc6e00bf3U, 0xd5a79147U, 0x06ca6351U, 0x14292967U,
    0x27b70a85U, 0x2e1b2138U, 0x4d2c6dfcU, 0x53380d13U,
    0x650a7354U, 0x766a0abbU, 0x81c2c92eU, 0x92722c85U,
    0xa2bfe8a1U, 0xa81a664bU, 0xc24b8b70U, 0xc76c51a3U,
    0xd192e819U, 0xd6990624U, 0xf40e3585U, 0x106aa070U,
    0x19a4c116U, 0x1e376c08U, 0x2748774cU, 0x34b0bcb5U,
    0x391c0cb3U, 0x4ed8aa4aU, 0x5b9cca4fU, 0x682e6ff3U,
    0x748f82eeU, 0x78a5636fU, 0x84c87814U, 0x8cc70208U,
    0x90befffaU, 0xa4506cebU, 0xbef9a3f7U, 0xc67178f2U
};

std::uint32_t rotr(std::uint32_t value, unsigned amount) {
    return (value >> amount) | (value << (32U - amount));
}

void compress(std::array<std::uint32_t, 8>& state,
              const std::array<std::uint8_t, 64>& block) {
    std::array<std::uint32_t, 64> schedule{};
    for (std::size_t i = 0; i < 16; ++i) {
        const std::size_t offset = i * 4;
        schedule[i] = (static_cast<std::uint32_t>(block[offset]) << 24U) |
                      (static_cast<std::uint32_t>(block[offset + 1]) << 16U) |
                      (static_cast<std::uint32_t>(block[offset + 2]) << 8U) |
                      static_cast<std::uint32_t>(block[offset + 3]);
    }
    for (std::size_t i = 16; i < 64; ++i) {
        const std::uint32_t s0 = rotr(schedule[i - 15], 7U) ^
                                 rotr(schedule[i - 15], 18U) ^
                                 (schedule[i - 15] >> 3U);
        const std::uint32_t s1 = rotr(schedule[i - 2], 17U) ^
                                 rotr(schedule[i - 2], 19U) ^
                                 (schedule[i - 2] >> 10U);
        schedule[i] = schedule[i - 16] + s0 + schedule[i - 7] + s1;
    }

    std::uint32_t a = state[0];
    std::uint32_t b = state[1];
    std::uint32_t c = state[2];
    std::uint32_t d = state[3];
    std::uint32_t e = state[4];
    std::uint32_t f = state[5];
    std::uint32_t g = state[6];
    std::uint32_t h = state[7];

    for (std::size_t i = 0; i < 64; ++i) {
        const std::uint32_t s1 = rotr(e, 6U) ^ rotr(e, 11U) ^ rotr(e, 25U);
        const std::uint32_t ch = (e & f) ^ ((~e) & g);
        const std::uint32_t temp1 = h + s1 + ch + kRound[i] + schedule[i];
        const std::uint32_t s0 = rotr(a, 2U) ^ rotr(a, 13U) ^ rotr(a, 22U);
        const std::uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        const std::uint32_t temp2 = s0 + maj;
        h = g;
        g = f;
        f = e;
        e = d + temp1;
        d = c;
        c = b;
        b = a;
        a = temp1 + temp2;
    }

    state[0] += a;
    state[1] += b;
    state[2] += c;
    state[3] += d;
    state[4] += e;
    state[5] += f;
    state[6] += g;
    state[7] += h;
}

}  // namespace

Digest sha256(const void* data, std::size_t size) {
    std::array<std::uint32_t, 8> state = kInitial;
    const auto* bytes = static_cast<const std::uint8_t*>(data);
    std::size_t offset = 0;

    while (size - offset >= 64) {
        std::array<std::uint8_t, 64> block{};
        std::memcpy(block.data(), bytes + offset, block.size());
        compress(state, block);
        offset += 64;
    }

    std::array<std::uint8_t, 128> tail{};
    const std::size_t remainder = size - offset;
    if (remainder > 0) {
        std::memcpy(tail.data(), bytes + offset, remainder);
    }
    tail[remainder] = 0x80U;
    const std::size_t tail_size = remainder < 56 ? 64 : 128;
    const std::uint64_t bit_length = static_cast<std::uint64_t>(size) * 8U;
    for (std::size_t i = 0; i < 8; ++i) {
        tail[tail_size - 1 - i] =
            static_cast<std::uint8_t>((bit_length >> (i * 8U)) & 0xffU);
    }
    for (std::size_t block_offset = 0; block_offset < tail_size; block_offset += 64) {
        std::array<std::uint8_t, 64> block{};
        std::memcpy(block.data(), tail.data() + block_offset, block.size());
        compress(state, block);
    }

    Digest result{};
    for (std::size_t i = 0; i < state.size(); ++i) {
        result[i * 4] = static_cast<std::uint8_t>((state[i] >> 24U) & 0xffU);
        result[i * 4 + 1] = static_cast<std::uint8_t>((state[i] >> 16U) & 0xffU);
        result[i * 4 + 2] = static_cast<std::uint8_t>((state[i] >> 8U) & 0xffU);
        result[i * 4 + 3] = static_cast<std::uint8_t>(state[i] & 0xffU);
    }
    return result;
}

Digest sha256(const std::string& data) {
    return sha256(data.data(), data.size());
}

std::string digest_hex(const Digest& digest) {
    static constexpr char kHex[] = "0123456789abcdef";
    std::string result;
    result.resize(64);
    for (std::size_t i = 0; i < digest.size(); ++i) {
        result[i * 2] = kHex[digest[i] >> 4U];
        result[i * 2 + 1] = kHex[digest[i] & 0x0fU];
    }
    return result;
}

bool parse_digest_hex(const std::string& text, Digest& out) {
    if (text.size() != 64) {
        return false;
    }
    auto value = [](char c) -> int {
        if (c >= '0' && c <= '9') {
            return c - '0';
        }
        if (c >= 'a' && c <= 'f') {
            return c - 'a' + 10;
        }
        if (c >= 'A' && c <= 'F') {
            return c - 'A' + 10;
        }
        return -1;
    };
    for (std::size_t i = 0; i < 32; ++i) {
        const int high = value(text[i * 2]);
        const int low = value(text[i * 2 + 1]);
        if (high < 0 || low < 0) {
            return false;
        }
        out[i] = static_cast<std::uint8_t>((high << 4) | low);
    }
    return true;
}

void Sha256Builder::append(const void* data, std::size_t size) {
    bytes_.append(static_cast<const char*>(data), size);
}

void Sha256Builder::append(const std::string& text) {
    bytes_.append(text);
}

void Sha256Builder::append_u8(std::uint8_t value) {
    bytes_.push_back(static_cast<char>(value));
}

void Sha256Builder::append_u32(std::uint32_t value) {
    for (unsigned shift = 0; shift < 32; shift += 8) {
        bytes_.push_back(static_cast<char>((value >> shift) & 0xffU));
    }
}

void Sha256Builder::append_u64(std::uint64_t value) {
    for (unsigned shift = 0; shift < 64; shift += 8) {
        bytes_.push_back(static_cast<char>((value >> shift) & 0xffU));
    }
}

void Sha256Builder::append_f32_bits(float value) {
    std::uint32_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "float must be 32-bit");
    std::memcpy(&bits, &value, sizeof(bits));
    append_u32(bits);
}

Digest Sha256Builder::finish() const {
    return sha256(bytes_);
}

}  // namespace battle
