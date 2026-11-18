#include "hash.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <iomanip>
#include <sstream>

namespace cr {
namespace {

constexpr std::array<std::uint32_t, 64> kSha256K = {
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

std::uint32_t rotr(std::uint32_t x, unsigned n) {
    return (x >> n) | (x << (32u - n));
}

}  // namespace

Digest sha256(const std::uint8_t* data, std::size_t size) {
    std::array<std::uint32_t, 8> h = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u,
                                      0xa54ff53au, 0x510e527fu, 0x9b05688cu,
                                      0x1f83d9abu, 0x5be0cd19u};

    const std::size_t fullBlocks = size / 64;
    for (std::size_t block = 0; block < fullBlocks; ++block) {
        std::uint32_t w[64] = {};
        for (std::size_t i = 0; i < 16; ++i) {
            const std::size_t o = block * 64 + i * 4;
            w[i] = (static_cast<std::uint32_t>(data[o]) << 24u) |
                   (static_cast<std::uint32_t>(data[o + 1]) << 16u) |
                   (static_cast<std::uint32_t>(data[o + 2]) << 8u) |
                   static_cast<std::uint32_t>(data[o + 3]);
        }
        for (std::size_t i = 16; i < 64; ++i) {
            const std::uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3u);
            const std::uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10u);
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
        for (std::size_t i = 0; i < 64; ++i) {
            const std::uint32_t s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
            const std::uint32_t ch = (e & f) ^ ((~e) & g);
            const std::uint32_t temp1 = hh + s1 + ch + kSha256K[i] + w[i];
            const std::uint32_t s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
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

    std::vector<std::uint8_t> tail;
    const std::size_t rem = size % 64;
    tail.reserve(rem + 72);
    tail.insert(tail.end(), data + fullBlocks * 64, data + size);
    tail.push_back(0x80u);
    while ((tail.size() % 64) != 56) {
        tail.push_back(0u);
    }
    const std::uint64_t bits = static_cast<std::uint64_t>(size) * 8u;
    for (int i = 7; i >= 0; --i) {
        tail.push_back(static_cast<std::uint8_t>((bits >> (i * 8)) & 0xffu));
    }
    for (std::size_t block = 0; block < tail.size() / 64; ++block) {
        std::uint32_t w[64] = {};
        for (std::size_t i = 0; i < 16; ++i) {
            const std::size_t o = block * 64 + i * 4;
            w[i] = (static_cast<std::uint32_t>(tail[o]) << 24u) |
                   (static_cast<std::uint32_t>(tail[o + 1]) << 16u) |
                   (static_cast<std::uint32_t>(tail[o + 2]) << 8u) |
                   static_cast<std::uint32_t>(tail[o + 3]);
        }
        for (std::size_t i = 16; i < 64; ++i) {
            const std::uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3u);
            const std::uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10u);
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
        for (std::size_t i = 0; i < 64; ++i) {
            const std::uint32_t s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
            const std::uint32_t ch = (e & f) ^ ((~e) & g);
            const std::uint32_t temp1 = hh + s1 + ch + kSha256K[i] + w[i];
            const std::uint32_t s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
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
    for (std::size_t i = 0; i < 8; ++i) {
        out[i * 4] = static_cast<std::uint8_t>((h[i] >> 24u) & 0xffu);
        out[i * 4 + 1] = static_cast<std::uint8_t>((h[i] >> 16u) & 0xffu);
        out[i * 4 + 2] = static_cast<std::uint8_t>((h[i] >> 8u) & 0xffu);
        out[i * 4 + 3] = static_cast<std::uint8_t>(h[i] & 0xffu);
    }
    return out;
}

Digest sha256(std::string_view text) {
    return sha256(reinterpret_cast<const std::uint8_t*>(text.data()), text.size());
}

std::string hexDigest(const Digest& digest) {
    std::ostringstream out;
    out << std::hex << std::setfill('0');
    for (const std::uint8_t byte : digest) {
        out << std::setw(2) << static_cast<unsigned>(byte);
    }
    return out.str();
}

std::string sha256Hex(const std::uint8_t* data, std::size_t size) {
    return hexDigest(sha256(data, size));
}

std::string sha256Hex(std::string_view text) {
    return hexDigest(sha256(text));
}

namespace {
constexpr char kBase64[] =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
int base64Value(char c) {
    if (c >= 'A' && c <= 'Z') return c - 'A';
    if (c >= 'a' && c <= 'z') return c - 'a' + 26;
    if (c >= '0' && c <= '9') return c - '0' + 52;
    if (c == '+') return 62;
    if (c == '/') return 63;
    return -1;
}
}  // namespace

std::string base64Encode(const std::vector<std::uint8_t>& data) {
    std::string out;
    out.reserve(((data.size() + 2) / 3) * 4);
    for (std::size_t i = 0; i < data.size(); i += 3) {
        const std::uint32_t a = data[i];
        const std::uint32_t b = (i + 1 < data.size()) ? data[i + 1] : 0u;
        const std::uint32_t c = (i + 2 < data.size()) ? data[i + 2] : 0u;
        const std::uint32_t triple = (a << 16u) | (b << 8u) | c;
        out.push_back(kBase64[(triple >> 18u) & 0x3fu]);
        out.push_back(kBase64[(triple >> 12u) & 0x3fu]);
        out.push_back(i + 1 < data.size() ? kBase64[(triple >> 6u) & 0x3fu] : '=');
        out.push_back(i + 2 < data.size() ? kBase64[triple & 0x3fu] : '=');
    }
    return out;
}

bool base64Decode(const std::string& text, std::vector<std::uint8_t>& data) {
    data.clear();
    if ((text.size() % 4) != 0) return false;
    data.reserve((text.size() / 4) * 3);
    for (std::size_t i = 0; i < text.size(); i += 4) {
        const int a = base64Value(text[i]);
        const int b = base64Value(text[i + 1]);
        const int c = text[i + 2] == '=' ? 0 : base64Value(text[i + 2]);
        const int d = text[i + 3] == '=' ? 0 : base64Value(text[i + 3]);
        if (a < 0 || b < 0 || c < 0 || d < 0) return false;
        const std::uint32_t triple = (static_cast<std::uint32_t>(a) << 18u) |
                                     (static_cast<std::uint32_t>(b) << 12u) |
                                     (static_cast<std::uint32_t>(c) << 6u) |
                                     static_cast<std::uint32_t>(d);
        data.push_back(static_cast<std::uint8_t>((triple >> 16u) & 0xffu));
        if (text[i + 2] != '=') data.push_back(static_cast<std::uint8_t>((triple >> 8u) & 0xffu));
        if (text[i + 3] != '=') data.push_back(static_cast<std::uint8_t>(triple & 0xffu));
    }
    return true;
}

std::uint32_t crc32(const std::uint8_t* data, std::size_t size) {
    std::uint32_t crc = 0xffffffffu;
    for (std::size_t i = 0; i < size; ++i) {
        crc ^= data[i];
        for (int bit = 0; bit < 8; ++bit) {
            const std::uint32_t mask = 0u - (crc & 1u);
            crc = (crc >> 1u) ^ (0xedb88320u & mask);
        }
    }
    return ~crc;
}

}  // namespace cr

