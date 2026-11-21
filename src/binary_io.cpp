#include "terrain/binary_io.h"

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <system_error>

#ifndef _WIN32
#include <fcntl.h>
#include <unistd.h>
#else
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#endif

namespace terrain {
namespace {

void setError(std::string* error, const std::string& message) {
    if (error != nullptr) {
        *error = message;
    }
}

}  // namespace

bool readFile(const std::string& path, std::vector<std::uint8_t>* out,
              std::string* error) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        setError(error, "cannot open " + path);
        return false;
    }
    stream.seekg(0, std::ios::end);
    const std::streamoff length = stream.tellg();
    if (length < 0) {
        setError(error, "cannot size " + path);
        return false;
    }
    stream.seekg(0, std::ios::beg);
    out->resize(static_cast<std::size_t>(length));
    if (length > 0) {
        stream.read(reinterpret_cast<char*>(out->data()), length);
        if (!stream) {
            setError(error, "cannot read " + path);
            return false;
        }
    }
    return true;
}

bool writeAllToFile(const std::string& path,
                    const std::vector<std::uint8_t>& data, bool durable,
                    std::string* error) {
    std::ofstream stream(path, std::ios::binary | std::ios::trunc);
    if (!stream) {
        setError(error, "cannot create " + path);
        return false;
    }
    if (!data.empty()) {
        stream.write(reinterpret_cast<const char*>(data.data()),
                     static_cast<std::streamsize>(data.size()));
    }
    stream.flush();
    if (!stream) {
        setError(error, "cannot write " + path);
        return false;
    }
    stream.close();
    if (!stream) {
        setError(error, "cannot close " + path);
        return false;
    }
#ifndef _WIN32
    if (durable) {
        const int fd = ::open(path.c_str(), O_RDONLY);
        if (fd < 0) {
            setError(error, "cannot open for fsync " + path);
            return false;
        }
        const int sync_result = ::fsync(fd);
        ::close(fd);
        if (sync_result != 0) {
            setError(error, "fsync failed " + path);
            return false;
        }
    }
#else
    if (durable) {
        HANDLE handle = CreateFileA(path.c_str(), GENERIC_READ,
                                    FILE_SHARE_READ, nullptr, OPEN_EXISTING,
                                    FILE_ATTRIBUTE_NORMAL, nullptr);
        if (handle == INVALID_HANDLE_VALUE) {
            setError(error, "cannot open for flush " + path);
            return false;
        }
        const BOOL flushed = FlushFileBuffers(handle);
        CloseHandle(handle);
        if (!flushed) {
            setError(error, "FlushFileBuffers failed " + path);
            return false;
        }
    }
#endif
    return true;
}

bool writeFile(const std::string& path, const std::vector<std::uint8_t>& data,
               std::string* error) {
    return writeAllToFile(path, data, false, error);
}

bool writeFileDurable(const std::string& path,
                      const std::vector<std::uint8_t>& data,
                      std::string* error) {
    return writeAllToFile(path, data, true, error);
}

bool writeFileAtomic(const std::string& path,
                     const std::vector<std::uint8_t>& data,
                     std::string* error) {
    static std::atomic<std::uint64_t> sequence{0};
    const std::uint64_t id = sequence.fetch_add(1) + 1;
    const std::string temp =
        path + ".tmp." + std::to_string(id) + "." +
        std::to_string(static_cast<unsigned long long>(
            std::hash<std::string>{}(path)));
    if (!writeFileDurable(temp, data, error)) {
        removeFileIfExists(temp);
        return false;
    }
    if (!renamePath(temp, path, error)) {
        removeFileIfExists(temp);
        return false;
    }
    return true;
}

bool makeDirectories(const std::string& path, std::string* error) {
    std::error_code ec;
    const bool ok = std::filesystem::create_directories(path, ec);
    if (ec) {
        setError(error, "create_directories failed: " + ec.message());
        return false;
    }
    (void)ok;
    return true;
}

bool pathExists(const std::string& path) {
    std::error_code ec;
    return std::filesystem::exists(path, ec);
}

bool removeFileIfExists(const std::string& path) {
    std::error_code ec;
    std::filesystem::remove(path, ec);
    return !ec;
}

bool renamePath(const std::string& from, const std::string& to,
                std::string* error) {
#ifdef _WIN32
    if (!MoveFileExA(from.c_str(), to.c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        setError(error, "MoveFileEx failed " + from + " -> " + to);
        return false;
    }
    return true;
#else
    std::error_code ec;
    std::filesystem::rename(from, to, ec);
    if (ec) {
        setError(error, "rename " + from + " -> " + to + " failed: " +
                            ec.message());
        return false;
    }
    return true;
#endif
}

bool syncDirectory(const std::string& path, std::string* error) {
#ifndef _WIN32
    const int fd = ::open(path.c_str(), O_RDONLY | O_DIRECTORY);
    if (fd < 0) {
        setError(error, "cannot open directory " + path);
        return false;
    }
    const int result = ::fsync(fd);
    ::close(fd);
    if (result != 0) {
        setError(error, "directory fsync failed " + path);
        return false;
    }
#else
    (void)path;
    (void)error;
#endif
    return true;
}

std::string joinPath(const std::string& a, const std::string& b) {
    if (a.empty()) return b;
    const char last = a.back();
    if (last == '/' || last == '\\') return a + b;
    return a + "/" + b;
}

std::vector<std::string> listFiles(const std::string& directory) {
    std::vector<std::string> out;
    std::error_code ec;
    if (!std::filesystem::exists(directory, ec)) {
        return out;
    }
    for (const auto& entry :
         std::filesystem::directory_iterator(directory, ec)) {
        if (ec) break;
        if (entry.is_regular_file(ec)) {
            out.push_back(entry.path().string());
        }
    }
    std::sort(out.begin(), out.end());
    return out;
}

bool ByteReader::u8(std::uint8_t* value) {
    if (remaining() < 1) return false;
    *value = data_[offset_++];
    return true;
}

bool ByteReader::u32(std::uint32_t* value) {
    if (remaining() < 4) return false;
    const std::uint8_t* p = data_ + offset_;
    *value = static_cast<std::uint32_t>(p[0]) |
             (static_cast<std::uint32_t>(p[1]) << 8u) |
             (static_cast<std::uint32_t>(p[2]) << 16u) |
             (static_cast<std::uint32_t>(p[3]) << 24u);
    offset_ += 4;
    return true;
}

bool ByteReader::u64(std::uint64_t* value) {
    if (remaining() < 8) return false;
    const std::uint8_t* p = data_ + offset_;
    *value = static_cast<std::uint64_t>(p[0]) |
             (static_cast<std::uint64_t>(p[1]) << 8u) |
             (static_cast<std::uint64_t>(p[2]) << 16u) |
             (static_cast<std::uint64_t>(p[3]) << 24u) |
             (static_cast<std::uint64_t>(p[4]) << 32u) |
             (static_cast<std::uint64_t>(p[5]) << 40u) |
             (static_cast<std::uint64_t>(p[6]) << 48u) |
             (static_cast<std::uint64_t>(p[7]) << 56u);
    offset_ += 8;
    return true;
}

bool ByteReader::f32(float* value) {
    std::uint32_t bits = 0;
    if (!u32(&bits)) return false;
    std::memcpy(value, &bits, sizeof(bits));
    return true;
}

bool ByteReader::bytes(void* dst, std::size_t count) {
    if (remaining() < count) return false;
    if (count != 0) {
        std::memcpy(dst, data_ + offset_, count);
    }
    offset_ += count;
    return true;
}

bool ByteReader::skip(std::size_t count) {
    if (remaining() < count) return false;
    offset_ += count;
    return true;
}

void ByteWriter::u8(std::uint8_t value) { data_.push_back(value); }

void ByteWriter::u32(std::uint32_t value) {
    data_.push_back(static_cast<std::uint8_t>(value & 0xffu));
    data_.push_back(static_cast<std::uint8_t>((value >> 8u) & 0xffu));
    data_.push_back(static_cast<std::uint8_t>((value >> 16u) & 0xffu));
    data_.push_back(static_cast<std::uint8_t>((value >> 24u) & 0xffu));
}

void ByteWriter::u64(std::uint64_t value) {
    for (int i = 0; i < 8; ++i) {
        data_.push_back(
            static_cast<std::uint8_t>((value >> (8u * i)) & 0xffu));
    }
}

void ByteWriter::f32(float value) {
    std::uint32_t bits = 0;
    std::memcpy(&bits, &value, sizeof(bits));
    u32(bits);
}

void ByteWriter::bytes(const void* src, std::size_t count) {
    const auto* p = static_cast<const std::uint8_t*>(src);
    data_.insert(data_.end(), p, p + count);
}

bool readLittleF32(const std::uint8_t* data, std::size_t size,
                   std::size_t offset, float* out) {
    if (offset + 4 > size) return false;
    const std::uint32_t bits =
        static_cast<std::uint32_t>(data[offset]) |
        (static_cast<std::uint32_t>(data[offset + 1]) << 8u) |
        (static_cast<std::uint32_t>(data[offset + 2]) << 16u) |
        (static_cast<std::uint32_t>(data[offset + 3]) << 24u);
    std::memcpy(out, &bits, sizeof(bits));
    return true;
}

}  // namespace terrain

