#include "core/Format.h"

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstring>
#include <fstream>
#include <limits>
#include <locale>
#include <set>
#include <sstream>

namespace meshedit {
namespace {

float toFloat(double value) {
    if (value > static_cast<double>(std::numeric_limits<float>::max())) {
        return std::numeric_limits<float>::infinity();
    }
    if (value < -static_cast<double>(std::numeric_limits<float>::max())) {
        return -std::numeric_limits<float>::infinity();
    }
    return static_cast<float>(value);
}

void writeFloat(std::ostream& out, float value) {
    const char bytes[4] = {
        static_cast<char>(reinterpret_cast<const unsigned char*>(&value)[0]),
        static_cast<char>(reinterpret_cast<const unsigned char*>(&value)[1]),
        static_cast<char>(reinterpret_cast<const unsigned char*>(&value)[2]),
        static_cast<char>(reinterpret_cast<const unsigned char*>(&value)[3])};
    out.write(bytes, 4);
}

float readFloat(const unsigned char* bytes) {
    float value = 0.0f;
    std::memcpy(&value, bytes, sizeof(float));
    return value;
}

std::string stlName(const std::string& name) {
    std::string result;
    for (char ch : name) {
        if (ch == '\n' || ch == '\r' || ch == '\t') result.push_back('_');
        else result.push_back(ch);
    }
    return result.empty() ? "mesh" : result;
}

Vec3 faceNormal(const Mesh& mesh, const Face& face) {
    if (face.corners[0].position < 0 || face.corners[1].position < 0 ||
        face.corners[2].position < 0) {
        return {};
    }
    const Vec3& a = mesh.positions[static_cast<std::size_t>(face.corners[0].position)];
    const Vec3& b = mesh.positions[static_cast<std::size_t>(face.corners[1].position)];
    const Vec3& c = mesh.positions[static_cast<std::size_t>(face.corners[2].position)];
    const Vec3 geometric = normalized(cross(b - a, c - a));
    int normalIndex = face.corners[0].normal;
    if (normalIndex >= 0 && face.corners[1].normal == normalIndex &&
        face.corners[2].normal == normalIndex &&
        static_cast<std::size_t>(normalIndex) < mesh.normals.size() &&
        lengthSquared(mesh.normals[static_cast<std::size_t>(normalIndex)]) > 0.0) {
        return normalized(mesh.normals[static_cast<std::size_t>(normalIndex)]);
    }
    return geometric;
}

Result<LoadResult> parseAsciiStl(std::ifstream&& input) {
    Mesh mesh;
    mesh.groups = {"stl"};
    std::string line;
    int lineNumber = 0;
    bool sawSolid = false;
    bool sawFacet = false;
    bool ended = false;
    std::string pendingName;

    auto next = [&](std::string& value, int& number) -> bool {
        while (std::getline(input, value)) {
            ++lineNumber;
            if (!value.empty() && value.back() == '\r') value.pop_back();
            value = trim(value);
            if (value.empty()) continue;
            number = lineNumber;
            return true;
        }
        return false;
    };

    std::string content;
    int contentLine = 0;
    if (!next(content, contentLine)) {
        return Error{Code::ParseInvalid, "empty STL file", 1, {}};
    }
    auto tokens = splitWhitespace(content);
    if (tokens.empty() || tokens[0] != "solid") {
        return Error{Code::ParseInvalid, "ASCII STL must start with 'solid'", contentLine, {}};
    }
    sawSolid = true;
    pendingName = tokens.size() > 1 ? trim(content.substr(content.find("solid") + 5)) : "mesh";
    mesh.name = pendingName.empty() ? "mesh" : pendingName;

    while (next(content, contentLine)) {
        tokens = splitWhitespace(content);
        if (tokens.empty()) continue;
        if (tokens[0] == "endsolid") {
            ended = true;
            if (next(content, contentLine)) {
                return Error{Code::ParseInvalid,
                             "unexpected data after endsolid", contentLine, {}};
            }
            break;
        }
        if (tokens[0] != "facet") {
            return Error{Code::ParseInvalid, "expected 'facet'", contentLine, {}};
        }
        if (tokens.size() != 5 || tokens[1] != "normal") {
            return Error{Code::ParseInvalid,
                         "facet normal requires exactly 3 values", contentLine, {}};
        }
        Vec3 normal;
        if (!parseVec3(tokens[2], tokens[3], tokens[4], normal)) {
            return Error{Code::ParseInvalid, "invalid facet normal", contentLine, {}};
        }
        if (!next(content, contentLine) || trim(content) != "outer loop") {
            return Error{Code::ParseInvalid, "expected 'outer loop'",
                         contentLine, {}};
        }
        std::array<Vec3, 3> vertices;
        for (int i = 0; i < 3; ++i) {
            if (!next(content, contentLine)) {
                return Error{Code::ParseInvalid, "truncated facet", lineNumber, {}};
            }
            tokens = splitWhitespace(content);
            if (tokens.size() != 4 || tokens[0] != "vertex") {
                return Error{Code::ParseInvalid,
                             "each facet requires three 'vertex x y z' records",
                             contentLine, {}};
            }
            if (!parseVec3(tokens[1], tokens[2], tokens[3], vertices[static_cast<std::size_t>(i)])) {
                return Error{Code::ParseInvalid, "invalid STL vertex", contentLine, {}};
            }
        }
        if (!next(content, contentLine) || trim(content) != "endloop") {
            return Error{Code::ParseInvalid, "expected 'endloop'", contentLine, {}};
        }
        if (!next(content, contentLine) || trim(content) != "endfacet") {
            return Error{Code::ParseInvalid, "expected 'endfacet'", contentLine, {}};
        }

        const int base = static_cast<int>(mesh.positions.size());
        mesh.positions.push_back(vertices[0]);
        mesh.positions.push_back(vertices[1]);
        mesh.positions.push_back(vertices[2]);
        const int normalIndex = static_cast<int>(mesh.normals.size());
        mesh.normals.push_back(normal);
        Face face;
        face.corners = {Corner{base, -1, normalIndex},
                        Corner{base + 1, -1, normalIndex},
                        Corner{base + 2, -1, normalIndex}};
        face.group = "stl";
        face.sourceLine = contentLine;
        mesh.faces.push_back(face);
        sawFacet = true;
    }
    if (!sawSolid || !sawFacet) {
        return Error{Code::ParseInvalid, "ASCII STL contains no facets", 1, {}};
    }
    if (!ended) {
        return Error{Code::ParseInvalid, "ASCII STL is missing endsolid", lineNumber, {}};
    }

    LoadResult result;
    result.mesh = std::move(mesh);
    result.report.sourceFormat = MeshFormat::Stl;
    result.report.targetFormat = MeshFormat::Stl;
    result.report.sourceVersion = 2;
    result.report.targetVersion = 2;
    result.report.notes.push_back("ASCII STL loaded as independent triangle vertices");
    return Result<LoadResult>(std::move(result));
}

Result<LoadResult> parseBinaryStl(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) return Error{Code::IoError, "cannot open STL file: " + path, 0, {}};
    input.seekg(0, std::ios::end);
    const std::streamoff size = input.tellg();
    input.seekg(0, std::ios::beg);
    if (size < 84) {
        return Error{Code::ParseInvalid, "binary STL is shorter than its 84-byte header",
                     0, {}};
    }
    std::array<unsigned char, 84> header{};
    input.read(reinterpret_cast<char*>(header.data()), header.size());
    if (!input) return Error{Code::ParseInvalid, "truncated binary STL header", 0, {}};
    const std::uint32_t count = static_cast<std::uint32_t>(header[80]) |
        (static_cast<std::uint32_t>(header[81]) << 8) |
        (static_cast<std::uint32_t>(header[82]) << 16) |
        (static_cast<std::uint32_t>(header[83]) << 24);
    const std::uint64_t expected = 84ull + static_cast<std::uint64_t>(count) * 50ull;
    if (expected != static_cast<std::uint64_t>(size)) {
        return Error{Code::ParseInvalid,
                     "binary STL triangle count does not match file size", 0, {}};
    }
    if (count == 0) {
        return Error{Code::ParseInvalid, "binary STL contains no facets", 0, {}};
    }

    Mesh mesh;
    mesh.groups = {"stl"};
    mesh.positions.reserve(static_cast<std::size_t>(count) * 3);
    mesh.normals.reserve(count);
    mesh.faces.reserve(count);
    std::array<unsigned char, 50> triangle{};
    for (std::uint32_t i = 0; i < count; ++i) {
        input.read(reinterpret_cast<char*>(triangle.data()), triangle.size());
        if (!input) {
            return Error{Code::ParseInvalid, "truncated binary STL triangle data", 0, {}};
        }
        auto readComponent = [&](std::size_t offset) {
            return static_cast<double>(readFloat(triangle.data() + offset));
        };
        const Vec3 normal{readComponent(0), readComponent(4), readComponent(8)};
        Vec3 vertices[3] = {
            {readComponent(12), readComponent(16), readComponent(20)},
            {readComponent(24), readComponent(28), readComponent(32)},
            {readComponent(36), readComponent(40), readComponent(44)}};
        if (!finite(normal) || !finite(vertices[0]) || !finite(vertices[1]) ||
            !finite(vertices[2])) {
            return Error{Code::ParseInvalid, "binary STL contains a non-finite value",
                         0, {}};
        }
        const int base = static_cast<int>(mesh.positions.size());
        mesh.positions.push_back(vertices[0]);
        mesh.positions.push_back(vertices[1]);
        mesh.positions.push_back(vertices[2]);
        const int normalIndex = static_cast<int>(mesh.normals.size());
        mesh.normals.push_back(normal);
        Face face;
        face.corners = {Corner{base, -1, normalIndex},
                        Corner{base + 1, -1, normalIndex},
                        Corner{base + 2, -1, normalIndex}};
        face.group = "stl";
        face.sourceLine = 0;
        mesh.faces.push_back(face);
    }
    LoadResult result;
    result.mesh = std::move(mesh);
    result.report.sourceFormat = MeshFormat::Stl;
    result.report.targetFormat = MeshFormat::Stl;
    result.report.sourceVersion = 2;
    result.report.targetVersion = 2;
    result.report.notes.push_back("binary STL loaded as independent triangle vertices");
    return Result<LoadResult>(std::move(result));
}

} // namespace

Result<LoadResult> parseStl(const std::string& path, StlVariant variant) {
    if (variant == StlVariant::Binary) return parseBinaryStl(path);
    if (variant == StlVariant::Ascii) {
        std::ifstream input(path, std::ios::binary);
        if (!input) return Error{Code::IoError, "cannot open STL file: " + path, 0, {}};
        return parseAsciiStl(std::move(input));
    }

    std::ifstream input(path, std::ios::binary);
    if (!input) return Error{Code::IoError, "cannot open STL file: " + path, 0, {}};
    std::array<char, 512> prefix{};
    input.read(prefix.data(), prefix.size());
    const std::streamsize got = input.gcount();
    input.clear();
    input.seekg(0, std::ios::beg);
    const std::string start(prefix.data(), static_cast<std::size_t>(got));
    const std::string trimmedStart = trim(start);
    if (trimmedStart.rfind("solid", 0) == 0) {
        Result<LoadResult> ascii = parseAsciiStl(std::move(input));
        if (ascii.ok()) return ascii;
        Result<LoadResult> binary = parseBinaryStl(path);
        if (binary.ok()) return binary;
        return ascii;
    }
    return parseBinaryStl(path);
}

Result<ExportResult> writeStl(const Mesh& mesh, const std::string& path,
                              StlVariant variant) {
    if (mesh.faces.empty()) {
        return Error{Code::InvalidArgument, "cannot export an empty STL", 0, {}};
    }
    try {
        validateMeshIndices(mesh, "STL export");
    } catch (const std::exception& exception) {
        return Error{Code::InvalidArgument, exception.what(), 0, {}};
    }
    auto representable = [](double value) {
        return std::isfinite(value) &&
            std::fabs(value) <= static_cast<double>(std::numeric_limits<float>::max());
    };
    for (const Vec3& position : mesh.positions) {
        if (!representable(position.x) || !representable(position.y) ||
            !representable(position.z)) {
            return Error{Code::InvalidArgument,
                         "STL float32 coordinates cannot represent this mesh", 0, {}};
        }
    }
    if (variant == StlVariant::Auto) variant = StlVariant::Binary;

    ExportResult result;
    result.report.sourceFormat = MeshFormat::Obj;
    result.report.targetFormat = MeshFormat::Stl;
    result.report.sourceVersion = mesh.sourceVersion;
    result.report.targetVersion = 2;
    std::set<std::string> groupSet;
    bool hasMaterial = false;
    bool hasUv = false;
    for (const Face& face : mesh.faces) {
        groupSet.insert(face.group);
        hasMaterial = hasMaterial || !face.material.empty();
        for (const Corner& corner : face.corners) hasUv = hasUv || corner.texcoord >= 0;
    }
    LossItem loss;
    loss.code = Code::FormatLoss;
    loss.message = "STL stores only triangle geometry and a per-facet normal";
    if (!groupSet.empty()) {
        for (const std::string& group : groupSet) loss.affectedGroups.push_back(group);
        result.report.losses.push_back(loss);
    }
    if (hasMaterial) {
        LossItem materialLoss = loss;
        materialLoss.message = "STL does not store material assignments";
        result.report.losses.push_back(std::move(materialLoss));
    }
    if (hasUv) {
        LossItem uvLoss = loss;
        uvLoss.message = "STL does not store texture coordinates";
        result.report.losses.push_back(std::move(uvLoss));
    }
    result.report.notes.push_back(
        variant == StlVariant::Binary ? "binary STL writes float32 coordinates"
                                      : "ASCII STL writes decimal coordinates");
    result.report.notes.push_back(
        "per-facet normals are preserved when valid, otherwise computed geometrically");

    if (variant == StlVariant::Binary) {
        std::ofstream out(path, std::ios::binary | std::ios::trunc);
        if (!out) return Error{Code::IoError, "cannot create STL file: " + path, 0, {}};
        std::array<char, 80> header{};
        const std::string text = "MeshEditor deterministic binary STL " + stlName(mesh.name);
        std::memcpy(header.data(), text.data(), std::min(text.size(), header.size() - 1));
        out.write(header.data(), header.size());
        const std::uint32_t count = static_cast<std::uint32_t>(mesh.faces.size());
        const char countBytes[4] = {
            static_cast<char>(count & 0xff),
            static_cast<char>((count >> 8) & 0xff),
            static_cast<char>((count >> 16) & 0xff),
            static_cast<char>((count >> 24) & 0xff)};
        out.write(countBytes, 4);
        for (const Face& face : mesh.faces) {
            const Vec3 normal = faceNormal(mesh, face);
            writeFloat(out, toFloat(normal.x));
            writeFloat(out, toFloat(normal.y));
            writeFloat(out, toFloat(normal.z));
            for (const Corner& corner : face.corners) {
                const Vec3& p = mesh.positions[static_cast<std::size_t>(corner.position)];
                writeFloat(out, toFloat(p.x));
                writeFloat(out, toFloat(p.y));
                writeFloat(out, toFloat(p.z));
            }
            const char attribute[2] = {0, 0};
            out.write(attribute, 2);
        }
        out.flush();
        if (!out) return Error{Code::IoError, "failed while writing STL file", 0, {}};
        out.close();
        result.bytesWritten = 84 + static_cast<std::size_t>(count) * 50;
    } else {
        std::ofstream out(path, std::ios::binary | std::ios::trunc);
        if (!out) return Error{Code::IoError, "cannot create STL file: " + path, 0, {}};
        out.imbue(std::locale::classic());
        out << "solid " << stlName(mesh.name) << "\n";
        for (const Face& face : mesh.faces) {
            const Vec3 normal = faceNormal(mesh, face);
            auto f = [](double value) {
                return formatDouble(static_cast<double>(toFloat(value)), 9);
            };
            out << "  facet normal " << f(normal.x) << ' ' << f(normal.y) << ' '
                << f(normal.z) << "\n";
            out << "    outer loop\n";
            for (const Corner& corner : face.corners) {
                const Vec3& p = mesh.positions[static_cast<std::size_t>(corner.position)];
                out << "      vertex " << f(p.x) << ' ' << f(p.y) << ' ' << f(p.z) << "\n";
            }
            out << "    endloop\n";
            out << "  endfacet\n";
        }
        out << "endsolid " << stlName(mesh.name) << "\n";
        out.flush();
        if (!out) return Error{Code::IoError, "failed while writing STL file", 0, {}};
        out.close();
        std::ifstream sizeCheck(path, std::ios::binary | std::ios::ate);
        if (sizeCheck) result.bytesWritten = static_cast<std::size_t>(sizeCheck.tellg());
    }
    return Result<ExportResult>(std::move(result));
}

} // namespace meshedit
