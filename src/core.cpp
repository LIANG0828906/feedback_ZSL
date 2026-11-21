#include "internal.hpp"

#include <algorithm>
#include <array>
#include <cerrno>
#include <cctype>
#include <cmath>
#include <cstring>
#include <fstream>
#include <iomanip>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <system_error>

#ifndef _WIN32
#include <fcntl.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#else
#include <windows.h>
#include <io.h>
#endif

namespace meshrepair {

const char* errorCodeName(ErrorCode code) {
    switch (code) {
        case ErrorCode::None: return "OK";
        case ErrorCode::MeshInvalid: return "E_MESH_INVALID";
        case ErrorCode::ConfigInvalid: return "E_CONFIG_INVALID";
        case ErrorCode::BoundaryPending: return "E_BOUNDARY_PENDING";
        case ErrorCode::TopologyConflict: return "E_TOPOLOGY_CONFLICT";
        case ErrorCode::BudgetExceeded: return "E_BUDGET_EXCEEDED";
        case ErrorCode::RecoveryInvalid: return "E_RECOVERY_INVALID";
        case ErrorCode::Busy: return "E_BUSY";
        case ErrorCode::IoError: return "E_IO_ERROR";
        case ErrorCode::Cancelled: return "E_CANCELLED";
        case ErrorCode::Internal: return "E_INTERNAL";
    }
    return "E_INTERNAL";
}

namespace detail {

namespace {

constexpr size_t kCommonMagicSize = 8;
constexpr size_t kShaSize = 32;
constexpr char kPlanMagic[8] = {'M', 'R', 'P', 'L', 'A', 'N', '0', '1'};

uint64_t loadLe(const uint8_t* p, size_t count) {
    uint64_t value = 0;
    for (size_t i = 0; i < count; ++i) {
        value |= static_cast<uint64_t>(p[i]) << (8 * i);
    }
    return value;
}

void storeLe(uint8_t* p, uint64_t value, size_t count) {
    for (size_t i = 0; i < count; ++i) {
        p[i] = static_cast<uint8_t>((value >> (8 * i)) & 0xffu);
    }
}

bool checkedMul(uint64_t a, uint64_t b, uint64_t& out) {
    if (a != 0 && b > std::numeric_limits<uint64_t>::max() / a) return false;
    out = a * b;
    return true;
}

bool checkedAdd(uint64_t a, uint64_t b, uint64_t& out) {
    if (b > std::numeric_limits<uint64_t>::max() - a) return false;
    out = a + b;
    return true;
}

std::string trim(const std::string& input) {
    size_t first = 0;
    while (first < input.size() &&
           std::isspace(static_cast<unsigned char>(input[first])) != 0) {
        ++first;
    }
    size_t last = input.size();
    while (last > first &&
           std::isspace(static_cast<unsigned char>(input[last - 1])) != 0) {
        --last;
    }
    return input.substr(first, last - first);
}

bool parseU64(const std::string& value, uint64_t& out) {
    if (value.empty() || value[0] == '-') return false;
    try {
        size_t consumed = 0;
        unsigned long long parsed = std::stoull(value, &consumed, 10);
        if (consumed != value.size()) return false;
        out = static_cast<uint64_t>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parseDouble(const std::string& value, double& out) {
    try {
        size_t consumed = 0;
        out = std::stod(value, &consumed);
        return consumed == value.size() && std::isfinite(out);
    } catch (...) {
        return false;
    }
}

}  // namespace

void Writer::u16(uint16_t value) {
    bytes_.push_back(static_cast<uint8_t>(value & 0xffu));
    bytes_.push_back(static_cast<uint8_t>((value >> 8) & 0xffu));
}

void Writer::u32(uint32_t value) {
    for (int i = 0; i < 4; ++i) {
        bytes_.push_back(static_cast<uint8_t>((value >> (8 * i)) & 0xffu));
    }
}

void Writer::u64(uint64_t value) {
    for (int i = 0; i < 8; ++i) {
        bytes_.push_back(static_cast<uint8_t>((value >> (8 * i)) & 0xffu));
    }
}

void Writer::f64(double value) {
    uint64_t bits = 0;
    static_assert(sizeof(bits) == sizeof(value), "double must be 64-bit");
    std::memcpy(&bits, &value, sizeof(bits));
    u64(bits);
}

void Writer::raw(const void* data, size_t size) {
    const auto* bytes = static_cast<const uint8_t*>(data);
    bytes_.insert(bytes_.end(), bytes, bytes + size);
}

void Writer::string(const std::string& value) {
    u64(value.size());
    raw(value.data(), value.size());
}

bool Reader::need(size_t count) {
    if (!ok_ || count > size_ - pos_) {
        ok_ = false;
        return false;
    }
    return true;
}

bool Reader::u8(uint8_t& value) {
    if (!need(1)) return false;
    value = data_[pos_++];
    return true;
}

bool Reader::u16(uint16_t& value) {
    if (!need(2)) return false;
    value = static_cast<uint16_t>(loadLe(data_ + pos_, 2));
    pos_ += 2;
    return true;
}

bool Reader::u32(uint32_t& value) {
    if (!need(4)) return false;
    value = static_cast<uint32_t>(loadLe(data_ + pos_, 4));
    pos_ += 4;
    return true;
}

bool Reader::u64(uint64_t& value) {
    if (!need(8)) return false;
    value = loadLe(data_ + pos_, 8);
    pos_ += 8;
    return true;
}

bool Reader::i64(int64_t& value) {
    uint64_t raw = 0;
    if (!u64(raw)) return false;
    value = static_cast<int64_t>(raw);
    return true;
}

bool Reader::f64(double& value) {
    uint64_t bits = 0;
    if (!u64(bits)) return false;
    std::memcpy(&value, &bits, sizeof(value));
    return true;
}

bool Reader::raw(void* out, size_t size) {
    if (!need(size)) return false;
    std::memcpy(out, data_ + pos_, size);
    pos_ += size;
    return true;
}

bool Reader::string(std::string& value) {
    uint64_t size = 0;
    if (!u64(size)) return false;
    if (size > static_cast<uint64_t>(size_ - pos_)) {
        ok_ = false;
        return false;
    }
    value.assign(reinterpret_cast<const char*>(data_ + pos_), static_cast<size_t>(size));
    pos_ += static_cast<size_t>(size);
    return true;
}

Error makeError(
    ErrorCode code,
    std::string message,
    std::optional<uint64_t> chunk,
    std::optional<uint64_t> face,
    std::optional<uint64_t> vertex) {
    Error error;
    error.code = code;
    error.message = std::move(message);
    error.chunk = chunk;
    error.face = face;
    error.vertex = vertex;
    return error;
}

Result<std::vector<uint8_t>> readFileBytes(const std::string& path) {
    std::ifstream stream(path, std::ios::binary);
    if (!stream) {
        return Result<std::vector<uint8_t>>::failure(makeError(
            ErrorCode::IoError, "cannot open file: " + path));
    }
    stream.seekg(0, std::ios::end);
    std::streamoff length = stream.tellg();
    if (length < 0) {
        return Result<std::vector<uint8_t>>::failure(makeError(
            ErrorCode::IoError, "cannot determine file size: " + path));
    }
    stream.seekg(0, std::ios::beg);
    std::vector<uint8_t> bytes(static_cast<size_t>(length));
    if (length > 0) {
        stream.read(reinterpret_cast<char*>(bytes.data()), length);
    }
    if (!stream) {
        return Result<std::vector<uint8_t>>::failure(makeError(
            ErrorCode::IoError, "cannot read file: " + path));
    }
    return Result<std::vector<uint8_t>>::success(std::move(bytes));
}

std::string parentPath(const std::string& path) {
    std::filesystem::path p(path);
    auto parent = p.parent_path();
    return parent.empty() ? std::string(".") : parent.string();
}

std::string joinPath(const std::string& a, const std::string& b) {
    return (std::filesystem::path(a) / b).string();
}

Status ensureDirectory(const std::string& path) {
    std::error_code ec;
    if (std::filesystem::exists(path, ec)) {
        if (!std::filesystem::is_directory(path, ec)) {
            return Status::failure(ErrorCode::IoError, "not a directory: " + path);
        }
        return Status::success();
    }
    if (!std::filesystem::create_directories(path, ec) && ec) {
        return Status::failure(
            ErrorCode::IoError, "cannot create directory " + path + ": " + ec.message());
    }
    return Status::success();
}

Status atomicWriteFile(const std::string& path, const std::vector<uint8_t>& bytes) {
    const std::string parent = parentPath(path);
    Status dir_status = ensureDirectory(parent);
    if (!dir_status) return dir_status;

    static std::atomic<uint64_t> sequence{0};
#ifndef _WIN32
    const uint64_t pid = static_cast<uint64_t>(::getpid());
#else
    const uint64_t pid = static_cast<uint64_t>(::GetCurrentProcessId());
#endif
    const std::string temp =
        path + ".tmp." + std::to_string(pid) + "." +
        std::to_string(sequence.fetch_add(1, std::memory_order_relaxed));

#ifndef _WIN32
    int fd = ::open(temp.c_str(), O_WRONLY | O_CREAT | O_EXCL, 0644);
    if (fd < 0) {
        return Status::failure(
            ErrorCode::IoError,
            "cannot create temporary file " + temp + ": " + std::strerror(errno));
    }
    size_t written = 0;
    while (written < bytes.size()) {
        ssize_t result = ::write(fd, bytes.data() + written, bytes.size() - written);
        if (result < 0) {
            if (errno == EINTR) continue;
            const std::string message = std::strerror(errno);
            ::close(fd);
            ::unlink(temp.c_str());
            return Status::failure(
                ErrorCode::IoError, "write failed for " + temp + ": " + message);
        }
        written += static_cast<size_t>(result);
    }
    if (::fsync(fd) != 0) {
        const std::string message = std::strerror(errno);
        ::close(fd);
        ::unlink(temp.c_str());
        return Status::failure(
            ErrorCode::IoError, "fsync failed for " + temp + ": " + message);
    }
    if (::close(fd) != 0) {
        const std::string message = std::strerror(errno);
        ::unlink(temp.c_str());
        return Status::failure(
            ErrorCode::IoError, "close failed for " + temp + ": " + message);
    }
    if (::rename(temp.c_str(), path.c_str()) != 0) {
        const std::string message = std::strerror(errno);
        ::unlink(temp.c_str());
        return Status::failure(
            ErrorCode::IoError, "atomic rename failed for " + path + ": " + message);
    }
    int dir_fd = ::open(parent.c_str(), O_RDONLY | O_DIRECTORY);
    if (dir_fd >= 0) {
        ::fsync(dir_fd);
        ::close(dir_fd);
    }
#else
    {
        std::ofstream stream(temp, std::ios::binary | std::ios::trunc);
        if (!stream) {
            return Status::failure(
                ErrorCode::IoError, "cannot create temporary file: " + temp);
        }
        if (!bytes.empty()) {
            stream.write(
                reinterpret_cast<const char*>(bytes.data()),
                static_cast<std::streamsize>(bytes.size()));
        }
        stream.flush();
        if (!stream) {
            stream.close();
            std::filesystem::remove(temp);
            return Status::failure(ErrorCode::IoError, "write failed: " + temp);
        }
    }
    if (!MoveFileExA(temp.c_str(), path.c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        std::filesystem::remove(temp);
        return Status::failure(ErrorCode::IoError, "atomic rename failed: " + path);
    }
#endif
    return Status::success();
}

Status removeStaleTemps(const std::string& directory) {
    std::error_code ec;
    if (!std::filesystem::exists(directory, ec)) return Status::success();
    for (const auto& entry : std::filesystem::directory_iterator(directory, ec)) {
        if (ec) break;
        const std::string name = entry.path().filename().string();
        if (name.find(".tmp.") != std::string::npos) {
            std::filesystem::remove(entry.path(), ec);
            ec.clear();
        }
    }
    return Status::success();
}

SessionLock::~SessionLock() {
    release();
}

Status SessionLock::acquire(const std::string& path) {
    release();
    Status dir_status = ensureDirectory(parentPath(path));
    if (!dir_status) return dir_status;
#ifndef _WIN32
    fd_ = ::open(path.c_str(), O_RDWR | O_CREAT, 0644);
    if (fd_ < 0) {
        return Status::failure(
            ErrorCode::IoError,
            "cannot open session lock " + path + ": " + std::strerror(errno));
    }
    if (::flock(fd_, LOCK_EX | LOCK_NB) != 0) {
        const int saved = errno;
        ::close(fd_);
        fd_ = -1;
        if (saved == EWOULDBLOCK || saved == EAGAIN) {
            return Status::failure(
                ErrorCode::Busy, "another repair session owns this mesh");
        }
        return Status::failure(
            ErrorCode::IoError,
            "cannot lock session file " + path + ": " + std::strerror(saved));
    }
#else
    HANDLE handle = CreateFileA(
        path.c_str(),
        GENERIC_READ | GENERIC_WRITE,
        0,
        nullptr,
        OPEN_ALWAYS,
        FILE_ATTRIBUTE_NORMAL,
        nullptr);
    if (handle == INVALID_HANDLE_VALUE) {
        return Status::failure(
            ErrorCode::Busy, "another repair session owns this mesh");
    }
    fd_ = 1;
    path_ = path;
    native_handle_ = handle;
#endif
    path_ = path;
    return Status::success();
}

void SessionLock::release() {
#ifndef _WIN32
    if (fd_ >= 0) {
        ::flock(fd_, LOCK_UN);
        ::close(fd_);
        fd_ = -1;
    }
#else
    if (fd_ >= 0) {
        CloseHandle(static_cast<HANDLE>(native_handle_));
        fd_ = -1;
        native_handle_ = nullptr;
    }
#endif
}

// --------------------------------------------------------------------------
// SHA-256

namespace {

class Sha256 {
public:
    Sha256() { reset(); }

    void update(const uint8_t* data, size_t size) {
        total_bytes_ += size;
        while (size > 0) {
            const size_t take = std::min(size, sizeof(buffer_) - buffer_size_);
            std::memcpy(buffer_.data() + buffer_size_, data, take);
            buffer_size_ += take;
            data += take;
            size -= take;
            if (buffer_size_ == sizeof(buffer_)) {
                transform(buffer_.data());
                buffer_size_ = 0;
            }
        }
    }

    std::array<uint8_t, 32> final() {
        const uint64_t bit_length = total_bytes_ * 8ull;
        buffer_[buffer_size_++] = 0x80;
        if (buffer_size_ > 56) {
            while (buffer_size_ < 64) buffer_[buffer_size_++] = 0;
            transform(buffer_.data());
            buffer_size_ = 0;
        }
        while (buffer_size_ < 56) buffer_[buffer_size_++] = 0;
        for (int i = 7; i >= 0; --i) {
            buffer_[buffer_size_++] = static_cast<uint8_t>((bit_length >> (i * 8)) & 0xffu);
        }
        transform(buffer_.data());

        std::array<uint8_t, 32> digest{};
        for (size_t i = 0; i < 8; ++i) {
            digest[i * 4] = static_cast<uint8_t>((state_[i] >> 24) & 0xffu);
            digest[i * 4 + 1] = static_cast<uint8_t>((state_[i] >> 16) & 0xffu);
            digest[i * 4 + 2] = static_cast<uint8_t>((state_[i] >> 8) & 0xffu);
            digest[i * 4 + 3] = static_cast<uint8_t>(state_[i] & 0xffu);
        }
        return digest;
    }

private:
    static uint32_t rotr(uint32_t value, uint32_t count) {
        return (value >> count) | (value << (32 - count));
    }

    void reset() {
        state_ = {
            0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
            0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};
        buffer_size_ = 0;
        total_bytes_ = 0;
    }

    void transform(const uint8_t* block) {
        static constexpr uint32_t k[64] = {
            0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u,
            0x3956c25bu, 0x59f111f1u, 0x923f82a4u, 0xab1c5ed5u,
            0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u,
            0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u,
            0xe49b69c1u, 0xefbe4786u, 0x0fc19dc6u, 0x240ca1ccu,
            0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
            0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u,
            0xc6e00bf3u, 0xd5a79147u, 0x06ca6351u, 0x14292967u,
            0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u,
            0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u,
            0xa2bfe8a1u, 0xa81a664bu, 0xc24b8b70u, 0xc76c51a3u,
            0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
            0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u,
            0x391c0cb3u, 0x4ed8aa4au, 0x5b9cca4fu, 0x682e6ff3u,
            0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u,
            0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};
        uint32_t w[64]{};
        for (size_t i = 0; i < 16; ++i) {
            w[i] = (static_cast<uint32_t>(block[i * 4]) << 24) |
                   (static_cast<uint32_t>(block[i * 4 + 1]) << 16) |
                   (static_cast<uint32_t>(block[i * 4 + 2]) << 8) |
                   static_cast<uint32_t>(block[i * 4 + 3]);
        }
        for (size_t i = 16; i < 64; ++i) {
            const uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
            const uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
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
            const uint32_t s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
            const uint32_t ch = (e & f) ^ ((~e) & g);
            const uint32_t temp1 = h + s1 + ch + k[i] + w[i];
            const uint32_t s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
            const uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
            const uint32_t temp2 = s0 + maj;
            h = g;
            g = f;
            f = e;
            e = d + temp1;
            d = c;
            c = b;
            b = a;
            a = temp1 + temp2;
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

    std::array<uint32_t, 8> state_{};
    std::array<uint8_t, 64> buffer_{};
    size_t buffer_size_ = 0;
    uint64_t total_bytes_ = 0;
};

std::string hexDigest(const std::array<uint8_t, 32>& digest) {
    static constexpr char digits[] = "0123456789abcdef";
    std::string result;
    result.resize(64);
    for (size_t i = 0; i < digest.size(); ++i) {
        result[i * 2] = digits[digest[i] >> 4];
        result[i * 2 + 1] = digits[digest[i] & 0x0fu];
    }
    return result;
}

}  // namespace

}  // namespace detail

using detail::checkedAdd;
using detail::checkedMul;
using detail::loadLe;
using detail::makeError;
using detail::parseDouble;
using detail::parseU64;
using detail::storeLe;
using detail::trim;

std::string sha256Hex(const uint8_t* data, size_t size) {
    detail::Sha256 hasher;
    hasher.update(data, size);
    return detail::hexDigest(hasher.final());
}

std::string sha256Hex(const std::vector<uint8_t>& data) {
    return sha256Hex(data.data(), data.size());
}

std::string sha256Hex(const std::string& data) {
    return sha256Hex(reinterpret_cast<const uint8_t*>(data.data()), data.size());
}

namespace {

std::string configCanonical(const Config& config) {
    detail::Writer writer;
    writer.string("meshrepair-config-v1");
    writer.u32(config.parallelism);
    writer.u64(config.memory_budget_bytes);
    writer.u64(config.block_memory_bytes);
    writer.f64(config.chunk_size_x);
    writer.f64(config.chunk_size_y);
    writer.f64(config.chunk_size_z);
    writer.f64(config.weld_tolerance);
    writer.u32(config.min_component_faces);
    writer.f64(config.normal_tolerance_deg);
    writer.u64(config.temp_reserve_bytes);
    writer.string(config.temp_dir);
    return sha256Hex(writer.bytes());
}

}  // namespace

std::string detail::configHash(const Config& config) {
    return configCanonical(config);
}

Result<Config> parseConfig(const std::string& text) {
    const std::set<std::string> required = {
        "parallelism",
        "memory_budget_bytes",
        "block_memory_bytes",
        "chunk_size_x",
        "chunk_size_y",
        "chunk_size_z",
        "weld_tolerance"};
    const std::set<std::string> optional = {
        "min_component_faces",
        "normal_tolerance_deg",
        "temp_reserve_bytes",
        "temp_dir"};

    std::map<std::string, std::string> values;
    std::istringstream stream(text);
    std::string line;
    uint64_t line_number = 0;
    while (std::getline(stream, line)) {
        ++line_number;
        const std::string cleaned = trim(line);
        if (cleaned.empty() || cleaned[0] == '#') continue;
        const size_t equals = cleaned.find('=');
        if (equals == std::string::npos) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid,
                "line " + std::to_string(line_number) + " is not key=value"));
        }
        const std::string key = trim(cleaned.substr(0, equals));
        const std::string value = trim(cleaned.substr(equals + 1));
        if (key.empty() || value.empty()) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid,
                "empty key or value on line " + std::to_string(line_number)));
        }
        if (required.count(key) == 0 && optional.count(key) == 0) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "unknown configuration field: " + key));
        }
        if (values.count(key) != 0) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "duplicate configuration field: " + key));
        }
        values[key] = value;
    }
    for (const auto& key : required) {
        if (values.count(key) == 0) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "missing required field: " + key));
        }
    }

    Config config;
    uint64_t parsed_u64 = 0;
    double parsed_double = 0.0;

    if (!parseU64(values["parallelism"], parsed_u64) || parsed_u64 == 0 ||
        parsed_u64 > 4096) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "parallelism must be in [1, 4096]"));
    }
    config.parallelism = static_cast<uint32_t>(parsed_u64);
    if (!parseU64(values["memory_budget_bytes"], parsed_u64) || parsed_u64 == 0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "memory_budget_bytes must be positive"));
    }
    config.memory_budget_bytes = parsed_u64;
    if (!parseU64(values["block_memory_bytes"], parsed_u64) || parsed_u64 == 0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "block_memory_bytes must be positive"));
    }
    config.block_memory_bytes = parsed_u64;
    if (config.block_memory_bytes > config.memory_budget_bytes) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid,
            "block_memory_bytes must not exceed memory_budget_bytes"));
    }
    if (!parseDouble(values["chunk_size_x"], config.chunk_size_x) ||
        config.chunk_size_x <= 0.0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "chunk_size_x must be finite and positive"));
    }
    if (!parseDouble(values["chunk_size_y"], config.chunk_size_y) ||
        config.chunk_size_y <= 0.0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "chunk_size_y must be finite and positive"));
    }
    if (!parseDouble(values["chunk_size_z"], config.chunk_size_z) ||
        config.chunk_size_z <= 0.0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "chunk_size_z must be finite and positive"));
    }
    if (!parseDouble(values["weld_tolerance"], config.weld_tolerance) ||
        config.weld_tolerance < 0.0) {
        return Result<Config>::failure(makeError(
            ErrorCode::ConfigInvalid, "weld_tolerance must be finite and non-negative"));
    }

    if (values.count("min_component_faces") != 0) {
        if (!parseU64(values["min_component_faces"], parsed_u64)) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "min_component_faces must be an integer"));
        }
        if (parsed_u64 < 1 || parsed_u64 > UINT32_MAX) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "min_component_faces must be positive"));
        }
        config.min_component_faces = static_cast<uint32_t>(parsed_u64);
    }
    if (values.count("normal_tolerance_deg") != 0) {
        if (!parseDouble(values["normal_tolerance_deg"], parsed_double) ||
            parsed_double < 0.0 || parsed_double > 180.0) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid,
                "normal_tolerance_deg must be in [0, 180]"));
        }
        config.normal_tolerance_deg = parsed_double;
    }
    if (values.count("temp_reserve_bytes") != 0) {
        if (!parseU64(values["temp_reserve_bytes"], config.temp_reserve_bytes)) {
            return Result<Config>::failure(makeError(
                ErrorCode::ConfigInvalid, "temp_reserve_bytes must be an integer"));
        }
    }
    if (values.count("temp_dir") != 0) {
        config.temp_dir = values["temp_dir"];
    } else {
        std::error_code ec;
        config.temp_dir = std::filesystem::temp_directory_path(ec).string();
        if (ec) config.temp_dir = ".";
    }
    return Result<Config>::success(config);
}

Result<Config> loadConfig(const std::string& path) {
    auto bytes = detail::readFileBytes(path);
    if (!bytes) return Result<Config>::failure(bytes.error);
    const std::string text(bytes->begin(), bytes->end());
    return parseConfig(text);
}

std::string defaultConfigText() {
    return
        "# MeshRepair deterministic configuration\n"
        "parallelism=4\n"
        "memory_budget_bytes=268435456\n"
        "block_memory_bytes=67108864\n"
        "chunk_size_x=1.0\n"
        "chunk_size_y=1.0\n"
        "chunk_size_z=1.0\n"
        "weld_tolerance=0.000001\n"
        "min_component_faces=3\n"
        "normal_tolerance_deg=5.0\n"
        "temp_reserve_bytes=67108864\n";
}

Status writeDefaultConfig(const std::string& path) {
    const std::string text = defaultConfigText();
    return detail::atomicWriteFile(
        path, std::vector<uint8_t>(text.begin(), text.end()));
}

// --------------------------------------------------------------------------
// Binary mesh I/O

namespace {

constexpr char kMeshMagic[8] = {'M', 'R', 'M', 'E', 'S', 'H', '0', '1'};
constexpr uint32_t kMeshHeaderSize = 48;
constexpr uint32_t kMeshFormatVersion = 1;

Error invalidAt(
    const std::string& source,
    const std::string& message,
    std::optional<uint64_t> face = {},
    std::optional<uint64_t> vertex = {}) {
    Error error = makeError(ErrorCode::MeshInvalid, source + ": " + message);
    error.face = face;
    error.vertex = vertex;
    return error;
}

}  // namespace

Result<Mesh> parseMesh(const std::vector<uint8_t>& bytes, const std::string& source) {
    if (bytes.size() < kMeshHeaderSize) {
        return Result<Mesh>::failure(invalidAt(
            source, "file shorter than 48-byte header", std::nullopt, 0));
    }
    if (std::memcmp(bytes.data(), kMeshMagic, sizeof(kMeshMagic)) != 0) {
        return Result<Mesh>::failure(invalidAt(source, "bad mesh magic"));
    }
    const uint32_t version = static_cast<uint32_t>(detail::loadLe(bytes.data() + 8, 4));
    const uint16_t coord_type =
        static_cast<uint16_t>(detail::loadLe(bytes.data() + 12, 2));
    const uint16_t index_width =
        static_cast<uint16_t>(detail::loadLe(bytes.data() + 14, 2));
    const uint64_t vertex_count = detail::loadLe(bytes.data() + 16, 8);
    const uint64_t face_count = detail::loadLe(bytes.data() + 24, 8);
    const uint32_t flags =
        static_cast<uint32_t>(detail::loadLe(bytes.data() + 32, 4));
    const uint32_t header_size =
        static_cast<uint32_t>(detail::loadLe(bytes.data() + 36, 4));
    const uint64_t declared_payload = detail::loadLe(bytes.data() + 40, 8);

    if (version != kMeshFormatVersion) {
        return Result<Mesh>::failure(invalidAt(source, "unsupported mesh version"));
    }
    if (header_size != kMeshHeaderSize) {
        return Result<Mesh>::failure(invalidAt(source, "header_size must be 48"));
    }
    if (flags != 0) {
        return Result<Mesh>::failure(invalidAt(source, "mesh flags must be zero"));
    }
    if (coord_type != 0 && coord_type != 1) {
        return Result<Mesh>::failure(invalidAt(
            source, "coordinate type must be 0 (float32) or 1 (float64)"));
    }
    if (index_width != 4 && index_width != 8) {
        return Result<Mesh>::failure(invalidAt(
            source, "index width must be 4 or 8"));
    }
    const uint64_t coord_size = coord_type == 0 ? 4 : 8;
    uint64_t vertex_bytes = 0;
    uint64_t face_bytes = 0;
    uint64_t expected_payload = 0;
    if (!detail::checkedMul(vertex_count, 3 * coord_size, vertex_bytes) ||
        !detail::checkedMul(
            face_count, 3 * static_cast<uint64_t>(index_width), face_bytes) ||
        !detail::checkedAdd(vertex_bytes, face_bytes, expected_payload)) {
        return Result<Mesh>::failure(invalidAt(source, "mesh payload size overflow"));
    }
    if (declared_payload != expected_payload) {
        const uint64_t available = bytes.size() >= kMeshHeaderSize
                                       ? bytes.size() - kMeshHeaderSize
                                       : 0;
        std::optional<uint64_t> face;
        std::optional<uint64_t> vertex;
        if (available < vertex_bytes) {
            vertex = available / (3 * coord_size);
        } else {
            const uint64_t available_face_bytes = available - vertex_bytes;
            face = std::min<uint64_t>(
                face_count, available_face_bytes / (3ull * index_width));
        }
        return Result<Mesh>::failure(invalidAt(
            source,
            "header payload length " + std::to_string(declared_payload) +
                " does not match vertex/face counts (" +
                std::to_string(expected_payload) + ")",
            face,
            vertex));
    }
    const uint64_t actual_payload = bytes.size() - kMeshHeaderSize;
    if (actual_payload != expected_payload) {
        std::optional<uint64_t> face;
        std::optional<uint64_t> vertex;
        if (actual_payload < vertex_bytes) {
            vertex = actual_payload / (3 * coord_size);
        } else if (actual_payload < expected_payload) {
            const uint64_t available_face_bytes = actual_payload - vertex_bytes;
            face = std::min<uint64_t>(
                face_count, available_face_bytes / (3ull * index_width));
        } else {
            face = face_count;
        }
        return Result<Mesh>::failure(invalidAt(
            source,
            "file length " + std::to_string(bytes.size()) +
                " does not match declared face count",
            face,
            vertex));
    }

    Mesh mesh;
    mesh.source_coord_type = coord_type;
    mesh.source_index_width = index_width;
    mesh.vertices.resize(static_cast<size_t>(vertex_count));
    mesh.faces.resize(static_cast<size_t>(face_count));
    const uint8_t* payload = bytes.data() + kMeshHeaderSize;
    for (uint64_t i = 0; i < vertex_count; ++i) {
        const uint8_t* coordinate = payload + static_cast<size_t>(i * 3 * coord_size);
        for (int axis = 0; axis < 3; ++axis) {
            double value = 0.0;
            if (coord_type == 0) {
                const uint32_t bits =
                    static_cast<uint32_t>(detail::loadLe(coordinate + axis * 4, 4));
                float f = 0.0f;
                std::memcpy(&f, &bits, sizeof(f));
                value = static_cast<double>(f);
            } else {
                const uint64_t bits = detail::loadLe(coordinate + axis * 8, 8);
                std::memcpy(&value, &bits, sizeof(value));
            }
            if (!std::isfinite(value)) {
                return Result<Mesh>::failure(invalidAt(
                    source, "non-finite vertex coordinate", std::nullopt, i));
            }
            (&mesh.vertices[static_cast<size_t>(i)].x)[axis] = value;
        }
    }

    const uint8_t* face_data = payload + vertex_bytes;
    for (uint64_t i = 0; i < face_count; ++i) {
        const uint8_t* current =
            face_data + static_cast<size_t>(i * 3 * index_width);
        for (int corner = 0; corner < 3; ++corner) {
            const uint64_t index =
                detail::loadLe(current + corner * index_width, index_width);
            if (index >= vertex_count) {
                return Result<Mesh>::failure(invalidAt(
                    source,
                    "face " + std::to_string(i) + " references vertex " +
                        std::to_string(index) + " but vertex count is " +
                        std::to_string(vertex_count),
                    i,
                    index));
            }
            mesh.faces[static_cast<size_t>(i)].v[corner] = index;
        }
    }
    return Result<Mesh>::success(std::move(mesh));
}

Result<Mesh> loadMesh(const std::string& path) {
    auto bytes = detail::readFileBytes(path);
    if (!bytes) return Result<Mesh>::failure(bytes.error);
    return parseMesh(*bytes, path);
}

Result<std::vector<uint8_t>> serializeMesh(
    const Mesh& mesh,
    uint16_t coord_type,
    uint16_t index_width) {
    if (coord_type != 0 && coord_type != 1) {
        return Result<std::vector<uint8_t>>::failure(invalidAt(
            "serializeMesh", "coordinate type must be 0 or 1"));
    }
    if (index_width != 4 && index_width != 8) {
        return Result<std::vector<uint8_t>>::failure(invalidAt(
            "serializeMesh", "index width must be 4 or 8"));
    }
    if (index_width == 4 && mesh.vertices.size() > UINT32_MAX) {
        return Result<std::vector<uint8_t>>::failure(invalidAt(
            "serializeMesh", "too many vertices for 32-bit indices"));
    }
    for (size_t i = 0; i < mesh.vertices.size(); ++i) {
        const Vec3& p = mesh.vertices[i];
        if (!std::isfinite(p.x) || !std::isfinite(p.y) || !std::isfinite(p.z)) {
            return Result<std::vector<uint8_t>>::failure(invalidAt(
                "serializeMesh", "non-finite vertex", std::nullopt, i));
        }
    }
    for (size_t i = 0; i < mesh.faces.size(); ++i) {
        for (uint64_t index : mesh.faces[i].v) {
            if (index >= mesh.vertices.size()) {
                return Result<std::vector<uint8_t>>::failure(invalidAt(
                    "serializeMesh", "face index out of range", i, index));
            }
        }
    }

    const uint64_t coord_size = coord_type == 0 ? 4 : 8;
    const uint64_t vertex_bytes = mesh.vertices.size() * 3 * coord_size;
    const uint64_t face_bytes = mesh.faces.size() * 3 * index_width;
    const uint64_t payload = vertex_bytes + face_bytes;
    uint64_t total = 0;
    if (!checkedAdd(kMeshHeaderSize, payload, total) ||
        total > std::numeric_limits<size_t>::max()) {
        return Result<std::vector<uint8_t>>::failure(invalidAt(
            "serializeMesh", "mesh is too large"));
    }
    std::vector<uint8_t> bytes(static_cast<size_t>(total), 0);
    std::memcpy(bytes.data(), kMeshMagic, sizeof(kMeshMagic));
    storeLe(bytes.data() + 8, kMeshFormatVersion, 4);
    storeLe(bytes.data() + 12, coord_type, 2);
    storeLe(bytes.data() + 14, index_width, 2);
    storeLe(bytes.data() + 16, mesh.vertices.size(), 8);
    storeLe(bytes.data() + 24, mesh.faces.size(), 8);
    storeLe(bytes.data() + 32, 0, 4);
    storeLe(bytes.data() + 36, kMeshHeaderSize, 4);
    storeLe(bytes.data() + 40, payload, 8);

    uint8_t* cursor = bytes.data() + kMeshHeaderSize;
    for (const Vec3& p : mesh.vertices) {
        for (double value : {p.x, p.y, p.z}) {
            if (coord_type == 0) {
                const float f = static_cast<float>(value);
                uint32_t bits = 0;
                std::memcpy(&bits, &f, sizeof(f));
                storeLe(cursor, bits, 4);
                cursor += 4;
            } else {
                uint64_t bits = 0;
                std::memcpy(&bits, &value, sizeof(value));
                storeLe(cursor, bits, 8);
                cursor += 8;
            }
        }
    }
    for (const Face& face : mesh.faces) {
        for (uint64_t index : face.v) {
            storeLe(cursor, index, index_width);
            cursor += index_width;
        }
    }
    return Result<std::vector<uint8_t>>::success(std::move(bytes));
}

Status writeMeshAtomic(
    const Mesh& mesh,
    const std::string& path,
    uint16_t coord_type,
    uint16_t index_width) {
    auto bytes = serializeMesh(mesh, coord_type, index_width);
    if (!bytes) return Status(bytes.error);
    return detail::atomicWriteFile(path, *bytes);
}

// --------------------------------------------------------------------------
// Topology

Result<Topology> buildTopology(const Mesh& mesh) {
    Topology topology;
    topology.vertex_faces.resize(mesh.vertices.size());
    topology.vertex_neighbors.resize(mesh.vertices.size());
    topology.directed_edges.reserve(mesh.faces.size() * 3);

    for (size_t vertex = 0; vertex < mesh.vertices.size(); ++vertex) {
        const Vec3& p = mesh.vertices[vertex];
        if (!std::isfinite(p.x) || !std::isfinite(p.y) || !std::isfinite(p.z)) {
            return Result<Topology>::failure(makeError(
                ErrorCode::MeshInvalid,
                "non-finite vertex coordinate",
                {},
                {},
                vertex));
        }
    }
    for (size_t face_id = 0; face_id < mesh.faces.size(); ++face_id) {
        const Face& face = mesh.faces[face_id];
        for (int i = 0; i < 3; ++i) {
            if (face.v[static_cast<size_t>(i)] >= mesh.vertices.size()) {
                return Result<Topology>::failure(makeError(
                    ErrorCode::MeshInvalid,
                    "face index out of range",
                    {},
                    face_id,
                    face.v[static_cast<size_t>(i)]));
            }
        }
        for (int i = 0; i < 3; ++i) {
            const uint64_t from = face.v[static_cast<size_t>(i)];
            const uint64_t to = face.v[static_cast<size_t>((i + 1) % 3)];
            topology.vertex_faces[static_cast<size_t>(from)].push_back(face_id);
            topology.vertex_neighbors[static_cast<size_t>(from)].push_back(to);
            topology.directed_edges.push_back(
                DirectedEdge{from, to, face_id, static_cast<uint8_t>(i)});
            topology.edge_faces[EdgeKey::make(from, to)].push_back(face_id);
        }
    }
    for (auto& faces : topology.vertex_faces) {
        std::sort(faces.begin(), faces.end());
        faces.erase(std::unique(faces.begin(), faces.end()), faces.end());
    }
    for (auto& neighbors : topology.vertex_neighbors) {
        std::sort(neighbors.begin(), neighbors.end());
        neighbors.erase(std::unique(neighbors.begin(), neighbors.end()), neighbors.end());
    }
    uint64_t edge_index = 0;
    for (auto& entry : topology.edge_faces) {
        std::sort(entry.second.begin(), entry.second.end());
        entry.second.erase(
            std::unique(entry.second.begin(), entry.second.end()),
            entry.second.end());
        if (entry.second.size() == 1) {
            topology.boundary_edges.push_back(edge_index);
        } else if (entry.second.size() > 2) {
            topology.nonmanifold_edges.push_back(edge_index);
        }
        ++edge_index;
    }
    return Result<Topology>::success(std::move(topology));
}

// --------------------------------------------------------------------------
// JSON helpers

namespace {

std::string jsonEscape(const std::string& value) {
    std::string result;
    result.reserve(value.size() + 8);
    for (char c : value) {
        switch (c) {
            case '"': result += "\\\""; break;
            case '\\': result += "\\\\"; break;
            case '\n': result += "\\n"; break;
            case '\r': result += "\\r"; break;
            case '\t': result += "\\t"; break;
            default:
                if (static_cast<unsigned char>(c) < 0x20) {
                    std::ostringstream stream;
                    stream << "\\u" << std::hex << std::setw(4) << std::setfill('0')
                           << static_cast<int>(static_cast<unsigned char>(c));
                    result += stream.str();
                } else {
                    result += c;
                }
        }
    }
    return result;
}

std::string optionalNumber(const std::optional<uint64_t>& value) {
    return value.has_value() ? std::to_string(*value) : std::string("null");
}

}  // namespace

std::string errorToJson(const Error& error) {
    std::ostringstream stream;
    stream << "{\"code\":\"" << errorCodeName(error.code) << "\","
           << "\"message\":\"" << jsonEscape(error.message) << "\","
           << "\"chunk\":" << optionalNumber(error.chunk) << ","
           << "\"face\":" << optionalNumber(error.face) << ","
           << "\"vertex\":" << optionalNumber(error.vertex) << ","
           << "\"edge_a\":" << optionalNumber(error.edge_a) << ","
           << "\"edge_b\":" << optionalNumber(error.edge_b) << ","
           << "\"hole_id\":";
    if (error.hole_id) {
        stream << "\"" << jsonEscape(*error.hole_id) << "\"";
    } else {
        stream << "null";
    }
    stream << ",\"estimated_bytes\":" << optionalNumber(error.estimated_bytes) << "}";
    return stream.str();
}

}  // namespace meshrepair
