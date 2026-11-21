#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "terrain/status.h"

namespace terrain {

bool readFile(const std::string& path, std::vector<std::uint8_t>* out,
              std::string* error = nullptr);
bool writeFile(const std::string& path, const std::vector<std::uint8_t>& data,
               std::string* error = nullptr);
bool writeFileAtomic(const std::string& path,
                     const std::vector<std::uint8_t>& data,
                     std::string* error = nullptr);
bool writeFileDurable(const std::string& path,
                      const std::vector<std::uint8_t>& data,
                      std::string* error = nullptr);
bool makeDirectories(const std::string& path, std::string* error = nullptr);
bool pathExists(const std::string& path);
bool removeFileIfExists(const std::string& path);
bool renamePath(const std::string& from, const std::string& to,
                std::string* error = nullptr);
bool syncDirectory(const std::string& path, std::string* error = nullptr);
std::string joinPath(const std::string& a, const std::string& b);
std::vector<std::string> listFiles(const std::string& directory);

class ByteReader {
public:
    ByteReader(const std::uint8_t* data, std::size_t size)
        : data_(data), size_(size) {}

    bool u8(std::uint8_t* value);
    bool u32(std::uint32_t* value);
    bool u64(std::uint64_t* value);
    bool f32(float* value);
    bool bytes(void* dst, std::size_t count);
    bool skip(std::size_t count);
    std::size_t offset() const { return offset_; }
    std::size_t remaining() const { return size_ - offset_; }
    const std::uint8_t* current() const { return data_ + offset_; }

private:
    const std::uint8_t* data_;
    std::size_t size_;
    std::size_t offset_ = 0;
};

class ByteWriter {
public:
    void u8(std::uint8_t value);
    void u32(std::uint32_t value);
    void u64(std::uint64_t value);
    void f32(float value);
    void bytes(const void* src, std::size_t count);
    const std::vector<std::uint8_t>& data() const { return data_; }

private:
    std::vector<std::uint8_t> data_;
};

bool readLittleF32(const std::uint8_t* data, std::size_t size,
                   std::size_t offset, float* out);
bool writeAllToFile(const std::string& path,
                    const std::vector<std::uint8_t>& data, bool durable,
                    std::string* error);

}  // namespace terrain

