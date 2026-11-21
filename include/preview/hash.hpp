#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <type_traits>

namespace preview {

constexpr std::uint64_t kFnvOffsetBasis = 14695981039346656037ull;
constexpr std::uint64_t kFnvPrime = 1099511628211ull;

std::uint64_t fnv1a64(const void* data, std::size_t size,
                      std::uint64_t seed = kFnvOffsetBasis) noexcept;
std::uint64_t hashCombine(std::uint64_t seed, std::uint64_t value) noexcept;
std::string hex64(std::uint64_t value);

class HashBuilder {
public:
    void addBytes(const void* data, std::size_t size) noexcept;
    void addString(const std::string& value) noexcept;

    template <typename T>
    void addPod(const T& value) noexcept {
        static_assert(std::is_trivially_copyable<T>::value,
                      "HashBuilder::addPod requires a trivially copyable type");
        addBytes(&value, sizeof(value));
    }

    std::uint64_t value() const noexcept { return value_; }

private:
    std::uint64_t value_ = kFnvOffsetBasis;
};

}  // namespace preview
