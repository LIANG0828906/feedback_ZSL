#include "core/Project.h"

#include <array>
#include <cerrno>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <limits>
#include <stdexcept>
#include <system_error>

#ifdef _WIN32
#include <process.h>
#include <windows.h>
#else
#include <fcntl.h>
#include <unistd.h>
#endif

namespace meshedit {
namespace {

constexpr char kMagic[8] = {'M', 'E', 'S', 'H', 'E', 'D', 'P', '1'};

class Writer {
public:
    void u8(std::uint8_t value) { data_.push_back(value); }
    void i32(std::int32_t value) {
        const std::uint32_t bits = static_cast<std::uint32_t>(value);
        for (int i = 0; i < 4; ++i) data_.push_back(static_cast<unsigned char>((bits >> (i * 8)) & 0xff));
    }
    void u32(std::uint32_t value) {
        for (int i = 0; i < 4; ++i) data_.push_back(static_cast<unsigned char>((value >> (i * 8)) & 0xff));
    }
    void u64(std::uint64_t value) {
        for (int i = 0; i < 8; ++i) data_.push_back(static_cast<unsigned char>((value >> (i * 8)) & 0xff));
    }
    void f64(double value) {
        std::uint64_t bits = 0;
        static_assert(sizeof(bits) == sizeof(value), "unexpected double size");
        std::memcpy(&bits, &value, sizeof(bits));
        u64(bits);
    }
    void text(const std::string& value) {
        u64(static_cast<std::uint64_t>(value.size()));
        data_.insert(data_.end(), value.begin(), value.end());
    }
    void vec3(const Vec3& value) { f64(value.x); f64(value.y); f64(value.z); }
    void vec2(const Vec2& value) { f64(value.x); f64(value.y); }
    void mesh(const Mesh& value) {
        text(value.name);
        i32(value.sourceVersion);
        u8(value.welded ? 1 : 0);
        u64(value.positions.size());
        for (const Vec3& position : value.positions) vec3(position);
        u64(value.texcoords.size());
        for (const Vec2& texcoord : value.texcoords) vec2(texcoord);
        u64(value.normals.size());
        for (const Vec3& normal : value.normals) vec3(normal);
        u64(value.faces.size());
        for (const Face& face : value.faces) {
            for (const Corner& corner : face.corners) {
                i32(corner.position);
                i32(corner.texcoord);
                i32(corner.normal);
            }
            text(face.group);
            text(face.material);
            i32(face.sourceLine);
        }
        u64(value.groups.size());
        for (const std::string& group : value.groups) text(group);
        u64(value.materialLibraries.size());
        for (const std::string& library : value.materialLibraries) text(library);
    }
    void record(const EditRecord& record) {
        text(record.operation);
        text(record.detail);
        mesh(record.before);
        mesh(record.after);
        metrics(record.beforeMetrics);
        metrics(record.afterMetrics);
    }
    void metrics(const MeshMetrics& value) {
        u64(value.vertexCount);
        u64(value.faceCount);
        u64(value.triangleCornerCount);
        const bool valid = value.bounds.valid();
        u8(valid ? 1 : 0);
        if (valid) {
            vec3(value.bounds.min);
            vec3(value.bounds.max);
        }
        f64(value.volume);
        f64(value.surfaceArea);
    }

    std::vector<unsigned char>& data() { return data_; }

private:
    std::vector<unsigned char> data_;
};

class Reader {
public:
    Reader(const std::vector<unsigned char>& data) : data_(data) {}

    std::uint8_t u8() {
        require(1);
        return data_[offset_++];
    }
    std::uint32_t u32() {
        require(4);
        std::uint32_t value = 0;
        for (int i = 0; i < 4; ++i) value |= static_cast<std::uint32_t>(data_[offset_++]) << (i * 8);
        return value;
    }
    std::int32_t i32() { return static_cast<std::int32_t>(u32()); }
    std::uint64_t u64() {
        require(8);
        std::uint64_t value = 0;
        for (int i = 0; i < 8; ++i) value |= static_cast<std::uint64_t>(data_[offset_++]) << (i * 8);
        return value;
    }
    double f64() {
        const std::uint64_t bits = u64();
        double value = 0.0;
        std::memcpy(&value, &bits, sizeof(value));
        return value;
    }
    std::string text() {
        const std::uint64_t length = u64();
        if (length > static_cast<std::uint64_t>(std::numeric_limits<std::size_t>::max())) {
            throw std::runtime_error("project string length overflow");
        }
        require(static_cast<std::size_t>(length));
        std::string value(reinterpret_cast<const char*>(data_.data() + offset_),
                          static_cast<std::size_t>(length));
        offset_ += static_cast<std::size_t>(length);
        return value;
    }
    Vec3 vec3() { Vec3 value; value.x = f64(); value.y = f64(); value.z = f64(); return value; }
    Vec2 vec2() { Vec2 value; value.x = f64(); value.y = f64(); return value; }
    Mesh mesh() {
        Mesh value;
        value.name = text();
        value.sourceVersion = i32();
        value.welded = u8() != 0;
        const std::uint64_t positions = checkedCount();
        value.positions.reserve(static_cast<std::size_t>(positions));
        for (std::uint64_t i = 0; i < positions; ++i) value.positions.push_back(vec3());
        const std::uint64_t texcoords = checkedCount();
        value.texcoords.reserve(static_cast<std::size_t>(texcoords));
        for (std::uint64_t i = 0; i < texcoords; ++i) value.texcoords.push_back(vec2());
        const std::uint64_t normals = checkedCount();
        value.normals.reserve(static_cast<std::size_t>(normals));
        for (std::uint64_t i = 0; i < normals; ++i) value.normals.push_back(vec3());
        const std::uint64_t faces = checkedCount();
        value.faces.reserve(static_cast<std::size_t>(faces));
        for (std::uint64_t i = 0; i < faces; ++i) {
            Face face;
            for (Corner& corner : face.corners) {
                corner.position = i32();
                corner.texcoord = i32();
                corner.normal = i32();
            }
            face.group = text();
            face.material = text();
            face.sourceLine = i32();
            value.faces.push_back(std::move(face));
        }
        const std::uint64_t groups = checkedCount();
        value.groups.reserve(static_cast<std::size_t>(groups));
        for (std::uint64_t i = 0; i < groups; ++i) value.groups.push_back(text());
        const std::uint64_t libraries = checkedCount();
        value.materialLibraries.reserve(static_cast<std::size_t>(libraries));
        for (std::uint64_t i = 0; i < libraries; ++i) value.materialLibraries.push_back(text());
        return value;
    }
    MeshMetrics metrics() {
        MeshMetrics value;
        value.vertexCount = static_cast<std::size_t>(u64());
        value.faceCount = static_cast<std::size_t>(u64());
        value.triangleCornerCount = static_cast<std::size_t>(u64());
        if (u8() != 0) {
            value.bounds.min = vec3();
            value.bounds.max = vec3();
        }
        value.volume = f64();
        value.surfaceArea = f64();
        return value;
    }
    EditRecord record() {
        EditRecord value;
        value.operation = text();
        value.detail = text();
        value.before = mesh();
        value.after = mesh();
        value.beforeMetrics = metrics();
        value.afterMetrics = metrics();
        return value;
    }
    bool finished() const { return offset_ == data_.size(); }

private:
    void require(std::size_t count) const {
        if (count > data_.size() || offset_ > data_.size() - count) {
            throw std::runtime_error("truncated project payload");
        }
    }
    std::uint64_t checkedCount() {
        const std::uint64_t count = u64();
        constexpr std::uint64_t limit = 100000000ull;
        if (count > limit) throw std::runtime_error("project count exceeds safety limit");
        return count;
    }

    const std::vector<unsigned char>& data_;
    std::size_t offset_ = 0;
};

void appendU32(std::vector<unsigned char>& out, std::uint32_t value) {
    for (int i = 0; i < 4; ++i) out.push_back(static_cast<unsigned char>((value >> (i * 8)) & 0xff));
}
void appendU64(std::vector<unsigned char>& out, std::uint64_t value) {
    for (int i = 0; i < 8; ++i) out.push_back(static_cast<unsigned char>((value >> (i * 8)) & 0xff));
}
std::uint32_t readU32(const unsigned char* data) {
    return static_cast<std::uint32_t>(data[0]) |
        (static_cast<std::uint32_t>(data[1]) << 8) |
        (static_cast<std::uint32_t>(data[2]) << 16) |
        (static_cast<std::uint32_t>(data[3]) << 24);
}
std::uint64_t readU64(const unsigned char* data) {
    std::uint64_t value = 0;
    for (int i = 0; i < 8; ++i) value |= static_cast<std::uint64_t>(data[i]) << (i * 8);
    return value;
}

std::string tempPathFor(const std::string& path) {
#ifdef _WIN32
    const int pid = _getpid();
#else
    const int pid = static_cast<int>(::getpid());
#endif
    return path + ".tmp." + std::to_string(pid);
}

} // namespace

std::uint64_t ProjectIo::contentHash(const std::vector<unsigned char>& payload) {
    std::uint64_t hash = 1469598103934665603ull;
    for (unsigned char byte : payload) {
        hash ^= byte;
        hash *= 1099511628211ull;
    }
    return hash;
}

Result<bool> ProjectIo::saveAtomic(const std::string& path, const ProjectData& project) {
    Writer writer;
    writer.mesh(project.current);
    writer.mesh(project.baseline);
    writer.i32(project.exportVersion);
    writer.f64(project.weldTolerance);
    writer.f64(project.validation.distanceTolerance);
    writer.i32(project.validation.samplingDensity);
    writer.f64(project.validation.normalAngleToleranceDegrees);
    writer.u64(project.undoHistory.size());
    for (const EditRecord& record : project.undoHistory) writer.record(record);
    writer.u64(project.redoHistory.size());
    for (const EditRecord& record : project.redoHistory) writer.record(record);

    std::vector<unsigned char> file;
    file.insert(file.end(), kMagic, kMagic + 8);
    appendU32(file, kProjectVersion);
    appendU64(file, static_cast<std::uint64_t>(writer.data().size()));
    appendU64(file, contentHash(writer.data()));
    file.insert(file.end(), writer.data().begin(), writer.data().end());

    const std::string temp = tempPathFor(path);
    std::error_code ignored;
    std::filesystem::remove(temp, ignored);
    {
        std::ofstream out(temp, std::ios::binary | std::ios::trunc);
        if (!out) return Error{Code::IoError, "cannot create temporary project: " + temp, 0, {}};
        out.write(reinterpret_cast<const char*>(file.data()),
                  static_cast<std::streamsize>(file.size()));
        out.flush();
        if (!out) {
            out.close();
            std::filesystem::remove(temp, ignored);
            return Error{Code::IoError, "failed while writing temporary project", 0, {}};
        }
    }
#ifndef _WIN32
    const int fd = ::open(temp.c_str(), O_RDONLY);
    if (fd >= 0) {
        ::fsync(fd);
        ::close(fd);
    }
#endif

#ifdef _WIN32
    if (!MoveFileExA(temp.c_str(), path.c_str(),
                     MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH)) {
        std::filesystem::remove(temp, ignored);
        return Error{Code::IoError, "atomic project replace failed", 0, {}};
    }
#else
    if (::rename(temp.c_str(), path.c_str()) != 0) {
        const std::string reason = std::strerror(errno);
        std::filesystem::remove(temp, ignored);
        return Error{Code::IoError, "atomic project replace failed: " + reason, 0, {}};
    }
#endif
    return success();
}

Result<ProjectData> ProjectIo::load(const std::string& path) {
    std::ifstream input(path, std::ios::binary | std::ios::ate);
    if (!input) return Error{Code::IoError, "cannot open project: " + path, 0, {}};
    const std::streamoff size = input.tellg();
    if (size < 28) {
        return Error{Code::ProjectMismatch, "project is truncated or not a MeshEditor project",
                     0, {}};
    }
    input.seekg(0, std::ios::beg);
    std::vector<unsigned char> data(static_cast<std::size_t>(size));
    input.read(reinterpret_cast<char*>(data.data()), size);
    if (!input) return Error{Code::IoError, "failed while reading project", 0, {}};

    if (std::memcmp(data.data(), kMagic, 8) != 0) {
        return Error{Code::ProjectMismatch, "project magic does not match", 0, {}};
    }
    const std::uint32_t version = readU32(data.data() + 8);
    if (version != kProjectVersion) {
        return Error{Code::ProjectMismatch,
                     "project version " + std::to_string(version) +
                         " does not match supported version " +
                         std::to_string(kProjectVersion),
                     0, {}};
    }
    const std::uint64_t payloadSize = readU64(data.data() + 12);
    const std::uint64_t expectedHash = readU64(data.data() + 20);
    if (payloadSize != data.size() - 28) {
        return Error{Code::ProjectMismatch, "project payload size is inconsistent", 0, {}};
    }
    std::vector<unsigned char> payload(data.begin() + 28, data.end());
    if (contentHash(payload) != expectedHash) {
        return Error{Code::ProjectMismatch, "project content hash does not match", 0, {}};
    }

    try {
        Reader reader(payload);
        ProjectData project;
        project.current = reader.mesh();
        project.baseline = reader.mesh();
        project.exportVersion = reader.i32();
        project.weldTolerance = reader.f64();
        project.validation.distanceTolerance = reader.f64();
        project.validation.samplingDensity = reader.i32();
        project.validation.normalAngleToleranceDegrees = reader.f64();
        if ((project.exportVersion != 1 && project.exportVersion != 2) ||
            !std::isfinite(project.weldTolerance) || project.weldTolerance < 0.0 ||
            !std::isfinite(project.validation.distanceTolerance) ||
            project.validation.distanceTolerance < 0.0 ||
            project.validation.samplingDensity < 1 ||
            project.validation.samplingDensity > 64 ||
            !std::isfinite(project.validation.normalAngleToleranceDegrees) ||
            project.validation.normalAngleToleranceDegrees < 0.0) {
            throw std::runtime_error("project contains invalid settings");
        }
        const std::uint64_t undoCount = reader.u64();
        if (undoCount > 1000000) throw std::runtime_error("undo history is too large");
        project.undoHistory.reserve(static_cast<std::size_t>(undoCount));
        for (std::uint64_t i = 0; i < undoCount; ++i) project.undoHistory.push_back(reader.record());
        const std::uint64_t redoCount = reader.u64();
        if (redoCount > 1000000) throw std::runtime_error("redo history is too large");
        project.redoHistory.reserve(static_cast<std::size_t>(redoCount));
        for (std::uint64_t i = 0; i < redoCount; ++i) project.redoHistory.push_back(reader.record());
        if (!reader.finished()) throw std::runtime_error("project has trailing payload bytes");
        validateMeshIndices(project.current, "project current mesh");
        validateMeshIndices(project.baseline, "project baseline mesh");
        for (const EditRecord& record : project.undoHistory) {
            validateMeshIndices(record.before, "project undo before mesh");
            validateMeshIndices(record.after, "project undo after mesh");
        }
        for (const EditRecord& record : project.redoHistory) {
            validateMeshIndices(record.before, "project redo before mesh");
            validateMeshIndices(record.after, "project redo after mesh");
        }
        return Result<ProjectData>(std::move(project));
    } catch (const std::exception& exception) {
        return Error{Code::ProjectMismatch,
                     std::string("project payload is invalid: ") + exception.what(), 0, {}};
    }
}

} // namespace meshedit
