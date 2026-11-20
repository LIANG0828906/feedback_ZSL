#pragma once

#include <glm/vec3.hpp>

#include "battle/bits.h"

#include <cstddef>
#include <cstdint>
#include <cstring>
#include <string>

namespace battle {

class ByteWriter {
public:
    void u8(std::uint8_t value) { data_.push_back(static_cast<char>(value)); }

    void u32(std::uint32_t value) {
        for (unsigned shift = 0; shift < 32; shift += 8) {
            data_.push_back(static_cast<char>((value >> shift) & 0xffU));
        }
    }

    void u64(std::uint64_t value) {
        for (unsigned shift = 0; shift < 64; shift += 8) {
            data_.push_back(static_cast<char>((value >> shift) & 0xffU));
        }
    }

    void f32(float value) { u32(float_bits(value)); }
    void vec3(const glm::vec3& value) {
        f32(value.x);
        f32(value.y);
        f32(value.z);
    }

    void bytes(const void* data, std::size_t size) {
        data_.append(static_cast<const char*>(data), size);
    }

    void bytes(const std::string& data) { data_.append(data); }

    const std::string& data() const noexcept { return data_; }
    std::string take() { return std::move(data_); }

private:
    std::string data_;
};

class ByteReader {
public:
    ByteReader(const void* data, std::size_t size)
        : data_(static_cast<const std::uint8_t*>(data)), size_(size) {}

    explicit ByteReader(const std::string& data) : ByteReader(data.data(), data.size()) {}

    bool u8(std::uint8_t& value) {
        if (!can_read(1)) {
            return false;
        }
        value = data_[offset_++];
        return true;
    }

    bool u32(std::uint32_t& value) {
        if (!can_read(4)) {
            return false;
        }
        value = static_cast<std::uint32_t>(data_[offset_]) |
                (static_cast<std::uint32_t>(data_[offset_ + 1]) << 8U) |
                (static_cast<std::uint32_t>(data_[offset_ + 2]) << 16U) |
                (static_cast<std::uint32_t>(data_[offset_ + 3]) << 24U);
        offset_ += 4;
        return true;
    }

    bool u64(std::uint64_t& value) {
        if (!can_read(8)) {
            return false;
        }
        value = 0;
        for (unsigned i = 0; i < 8; ++i) {
            value |= static_cast<std::uint64_t>(data_[offset_ + i]) << (i * 8U);
        }
        offset_ += 8;
        return true;
    }

    bool f32(float& value) {
        std::uint32_t bits = 0;
        if (!u32(bits)) {
            return false;
        }
        value = bits_float(bits);
        return true;
    }

    bool vec3(glm::vec3& value) {
        return f32(value.x) && f32(value.y) && f32(value.z);
    }

    bool bytes(void* destination, std::size_t size) {
        if (!can_read(size)) {
            return false;
        }
        std::memcpy(destination, data_ + offset_, size);
        offset_ += size;
        return true;
    }

    bool skip(std::size_t size) {
        if (!can_read(size)) {
            return false;
        }
        offset_ += size;
        return true;
    }

    std::size_t offset() const noexcept { return offset_; }
    std::size_t remaining() const noexcept { return size_ - offset_; }
    bool finished() const noexcept { return offset_ == size_; }

private:
    bool can_read(std::size_t size) const noexcept {
        return size <= size_ - offset_;
    }

    const std::uint8_t* data_ = nullptr;
    std::size_t size_ = 0;
    std::size_t offset_ = 0;
};

}  // namespace battle

