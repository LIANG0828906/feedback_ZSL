#include "core/Format.h"

#include <algorithm>
#include <fstream>
#include <locale>
#include <map>
#include <set>
#include <sstream>
#include <utility>

namespace meshedit {
namespace {

bool parseFaceCorner(const std::string& token, std::size_t positions,
                     std::size_t texcoords, std::size_t normals,
                     Corner& corner, std::string& why) {
    if (token.empty()) {
        why = "empty face corner";
        return false;
    }
    std::vector<std::string> fields;
    std::size_t start = 0;
    for (;;) {
        const std::size_t slash = token.find('/', start);
        fields.push_back(token.substr(start, slash == std::string::npos ? std::string::npos
                                                                        : slash - start));
        if (slash == std::string::npos) break;
        start = slash + 1;
        if (start > token.size()) break;
    }
    if (fields.size() > 3) {
        why = "face corner has more than three indices";
        return false;
    }

    auto resolve = [](const std::string& text, std::size_t count, int& out) -> bool {
        if (text.empty()) {
            out = -1;
            return true;
        }
        int parsed = 0;
        if (!parseInteger(text, parsed) || parsed == 0) return false;
        const long long resolved = parsed > 0
            ? static_cast<long long>(parsed - 1)
            : static_cast<long long>(count) + parsed;
        if (resolved < 0 || static_cast<std::size_t>(resolved) >= count) return false;
        out = static_cast<int>(resolved);
        return true;
    };

    if (!resolve(fields[0], positions, corner.position)) {
        why = "position index is out of range or malformed";
        return false;
    }
    if (corner.position < 0) {
        why = "face corner is missing a position index";
        return false;
    }
    if (fields.size() >= 2) {
        if (fields[1].empty() && fields.size() == 2) {
            why = "texture coordinate index is missing after '/'";
            return false;
        }
        if (!resolve(fields[1], texcoords, corner.texcoord)) {
            why = "texture coordinate index is out of range or malformed";
            return false;
        }
    }
    if (fields.size() == 3) {
        if (fields[2].empty()) {
            why = "normal index is missing after '//'";
            return false;
        }
        if (!resolve(fields[2], normals, corner.normal)) {
            why = "normal index is out of range or malformed";
            return false;
        }
    }
    if (fields.size() == 2 && !fields[1].empty() && corner.texcoord < 0) {
        why = "texture coordinate index is invalid";
        return false;
    }
    return true;
}

void addGroup(Mesh& mesh, const std::string& group) {
    if (std::find(mesh.groups.begin(), mesh.groups.end(), group) == mesh.groups.end()) {
        mesh.groups.push_back(group);
    }
}

std::string escapeObjName(const std::string& input) {
    std::string result;
    result.reserve(input.size());
    for (char ch : input) {
        if (ch == '\n' || ch == '\r' || ch == '\t') result.push_back('_');
        else result.push_back(ch);
    }
    if (result.empty()) result = "unnamed";
    return result;
}

void writeVec(std::ostream& out, const Vec3& value) {
    out << formatDouble(value.x) << ' ' << formatDouble(value.y) << ' '
        << formatDouble(value.z);
}

} // namespace

Result<LoadResult> parseObj(const std::string& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) {
        return Error{Code::IoError, "cannot open OBJ file: " + path, 0, {}};
    }

    Mesh mesh;
    mesh.groups.clear();
    std::string currentGroup = "default";
    std::string currentMaterial;
    addGroup(mesh, currentGroup);
    std::string line;
    int lineNumber = 0;
    bool sawObject = false;
    std::size_t triangulatedPolygons = 0;

    while (std::getline(input, line)) {
        ++lineNumber;
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (lineNumber == 1 && line.size() >= 3 &&
            static_cast<unsigned char>(line[0]) == 0xEF &&
            static_cast<unsigned char>(line[1]) == 0xBB &&
            static_cast<unsigned char>(line[2]) == 0xBF) {
            line.erase(0, 3);
        }
        const std::string content = trim(line);
        if (content.empty()) continue;
        if (content[0] == '#') {
            const std::string comment = trim(content.substr(1));
            const std::string prefix = "mesh-editor-version";
            if (comment.compare(0, prefix.size(), prefix) == 0) {
                const int value = [&] {
                    const std::string text = trim(comment.substr(prefix.size()));
                    int parsed = 0;
                    return parseInteger(text, parsed) ? parsed : 0;
                }();
                if (value != 1 && value != 2) {
                    return Error{Code::ParseInvalid,
                                 "unsupported mesh-editor OBJ version",
                                 lineNumber, {}};
                }
                mesh.sourceVersion = value;
            }
            continue;
        }

        auto tokens = splitWhitespace(content);
        if (tokens.empty()) continue;
        const std::string& keyword = tokens[0];
        if (keyword == "v") {
            if (tokens.size() < 4) {
                return Error{Code::ParseInvalid, "OBJ vertex requires 3 coordinates",
                             lineNumber, {}};
            }
            Vec3 value;
            if (!parseVec3(tokens[1], tokens[2], tokens[3], value)) {
                return Error{Code::ParseInvalid, "invalid OBJ vertex coordinate",
                             lineNumber, {}};
            }
            mesh.positions.push_back(value);
        } else if (keyword == "vt") {
            if (tokens.size() < 3) {
                return Error{Code::ParseInvalid,
                             "OBJ texture coordinate requires at least 2 values",
                             lineNumber, {}};
            }
            Vec2 value;
            if (!parseVec2(tokens[1], tokens[2], value)) {
                return Error{Code::ParseInvalid, "invalid OBJ texture coordinate",
                             lineNumber, {}};
            }
            mesh.texcoords.push_back(value);
        } else if (keyword == "vn") {
            if (tokens.size() < 4) {
                return Error{Code::ParseInvalid, "OBJ normal requires 3 components",
                             lineNumber, {}};
            }
            Vec3 value;
            if (!parseVec3(tokens[1], tokens[2], tokens[3], value)) {
                return Error{Code::ParseInvalid, "invalid OBJ normal", lineNumber, {}};
            }
            mesh.normals.push_back(value);
        } else if (keyword == "f") {
            if (tokens.size() < 4) {
                return Error{Code::ParseInvalid,
                             "OBJ face requires at least 3 corners", lineNumber, {}};
            }
            std::vector<Corner> corners;
            corners.reserve(tokens.size() - 1);
            for (std::size_t i = 1; i < tokens.size(); ++i) {
                Corner corner;
                std::string why;
                if (!parseFaceCorner(tokens[i], mesh.positions.size(), mesh.texcoords.size(),
                                     mesh.normals.size(), corner, why)) {
                    return Error{Code::ParseInvalid, "invalid OBJ face: " + why,
                                 lineNumber, {}};
                }
                corner.position = static_cast<int>(
                    corner.position < 0
                        ? static_cast<long long>(mesh.positions.size()) + corner.position
                        : corner.position);
                corners.push_back(corner);
            }
            // parseFaceCorner already resolves relative indices against the current counts.
            if (corners.size() > 3) ++triangulatedPolygons;
            for (std::size_t i = 1; i + 1 < corners.size(); ++i) {
                Face face;
                face.corners = {corners[0], corners[i], corners[i + 1]};
                face.group = currentGroup;
                face.material = currentMaterial;
                face.sourceLine = lineNumber;
                mesh.faces.push_back(face);
            }
        } else if (keyword == "g") {
            currentGroup = tokens.size() > 1 ? trim(content.substr(content.find("g") + 1))
                                             : "default";
            if (currentGroup.empty()) currentGroup = "default";
            addGroup(mesh, currentGroup);
        } else if (keyword == "o") {
            const std::string objectName =
                tokens.size() > 1 ? trim(content.substr(content.find("o") + 1)) : "mesh";
            if (!sawObject) mesh.name = objectName.empty() ? "mesh" : objectName;
            sawObject = true;
        } else if (keyword == "usemtl") {
            currentMaterial = tokens.size() > 1
                ? trim(content.substr(content.find("usemtl") + 6))
                : std::string();
            if (currentMaterial == "off") currentMaterial.clear();
        } else if (keyword == "mtllib") {
            if (tokens.size() > 1) {
                mesh.materialLibraries.push_back(
                    trim(content.substr(content.find("mtllib") + 6)));
            }
        }
    }

    if (!input.eof()) {
        return Error{Code::IoError, "failed while reading OBJ file", lineNumber, {}};
    }
    if (mesh.faces.empty()) {
        return Error{Code::ParseInvalid, "OBJ contains no faces", lineNumber, {}};
    }
    if (mesh.groups.empty()) mesh.groups.push_back("default");

    LoadResult result;
    result.mesh = std::move(mesh);
    result.report.sourceFormat = MeshFormat::Obj;
    result.report.targetFormat = MeshFormat::Obj;
    result.report.sourceVersion = result.mesh.sourceVersion;
    result.report.targetVersion = 2;
    if (triangulatedPolygons > 0) {
        result.report.notes.push_back(
            "triangulated " + std::to_string(triangulatedPolygons) +
            " OBJ polygon(s) with a deterministic fan");
    }
    if (result.mesh.sourceVersion == 1) {
        result.report.upgradedOnLoad = true;
        result.report.notes.push_back(
            "v1 vertex-bound UV/normal rule upgraded to v2 per-corner indices");
    }
    return Result<LoadResult>(std::move(result));
}

Result<ExportResult> writeObj(const Mesh& mesh, const std::string& path, Version version) {
    if (mesh.faces.empty()) {
        return Error{Code::InvalidArgument, "cannot export an empty OBJ", 0, {}};
    }
    try {
        validateMeshIndices(mesh, "OBJ export");
    } catch (const std::exception& exception) {
        return Error{Code::InvalidArgument, exception.what(), 0, {}};
    }

    if (version == Version::V1) {
        Result<ConversionReport> check = checkObjVersionLoss(mesh, version);
        if (!check.ok()) return check.error;
    }

    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) {
        return Error{Code::IoError, "cannot create OBJ file: " + path, 0, {}};
    }
    out.imbue(std::locale::classic());
    out << "# MeshEditor OBJ\n";
    out << "# mesh-editor-version " << versionNumber(version) << "\n";
    out << "o " << escapeObjName(mesh.name) << "\n";
    for (const std::string& library : mesh.materialLibraries) {
        if (!library.empty()) out << "mtllib " << library << "\n";
    }
    for (const Vec3& position : mesh.positions) {
        out << "v ";
        writeVec(out, position);
        out << "\n";
    }
    for (const Vec2& texcoord : mesh.texcoords) {
        out << "vt " << formatDouble(texcoord.x) << ' ' << formatDouble(texcoord.y) << "\n";
    }
    for (const Vec3& normal : mesh.normals) {
        out << "vn ";
        writeVec(out, normal);
        out << "\n";
    }

    std::string currentGroup;
    std::string currentMaterial;
    for (const Face& face : mesh.faces) {
        if (face.group != currentGroup) {
            out << "g " << escapeObjName(face.group) << "\n";
            currentGroup = face.group;
            currentMaterial.clear();
        }
        if (face.material != currentMaterial) {
            if (!face.material.empty()) out << "usemtl " << face.material << "\n";
            else out << "usemtl off\n";
            currentMaterial = face.material;
        }
        out << 'f';
        for (const Corner& corner : face.corners) {
            out << ' ' << (corner.position + 1);
            if (corner.texcoord >= 0 || corner.normal >= 0) {
                out << '/';
                if (corner.texcoord >= 0) out << (corner.texcoord + 1);
                if (corner.normal >= 0) out << '/' << (corner.normal + 1);
            }
        }
        out << '\n';
    }
    out.flush();
    if (!out) {
        return Error{Code::IoError, "failed while writing OBJ file: " + path, 0, {}};
    }
    out.close();

    ExportResult result;
    result.report.sourceFormat = MeshFormat::Obj;
    result.report.targetFormat = MeshFormat::Obj;
    result.report.sourceVersion = 2;
    result.report.targetVersion = versionNumber(version);
    std::ifstream sizeCheck(path, std::ios::binary | std::ios::ate);
    if (sizeCheck) result.bytesWritten = static_cast<std::size_t>(sizeCheck.tellg());
    return Result<ExportResult>(std::move(result));
}

Result<ConversionReport> checkObjVersionLoss(const Mesh& mesh, Version target) {
    ConversionReport report;
    report.sourceFormat = MeshFormat::Obj;
    report.targetFormat = MeshFormat::Obj;
    report.sourceVersion = 2;
    report.targetVersion = versionNumber(target);
    if (target != Version::V1) return Result<ConversionReport>(std::move(report));

    std::map<std::string, std::set<std::string>> groupMaterials;
    std::map<int, std::pair<int, int>> positionAttributes;
    std::map<std::string, std::set<std::string>> affected;
    for (const Face& face : mesh.faces) {
        groupMaterials[face.group].insert(face.material);
        for (const Corner& corner : face.corners) {
            if (corner.position < 0) continue;
            const auto key = std::make_pair(corner.texcoord, corner.normal);
            const auto found = positionAttributes.find(corner.position);
            if (found == positionAttributes.end()) {
                positionAttributes.emplace(corner.position, key);
            } else if (found->second != key) {
                affected[face.group].insert(face.group);
            }
        }
    }
    for (const auto& item : groupMaterials) {
        if (item.second.size() > 1) affected[item.first].insert(item.first);
    }

    if (!affected.empty()) {
        LossItem loss;
        loss.code = Code::VersionLoss;
        loss.message =
            "v1 binds one UV/normal pair per shared position and allows one material per group";
        for (const auto& item : affected) loss.affectedGroups.push_back(item.first);
        report.losses.push_back(std::move(loss));
        Error error;
        error.code = Code::VersionLoss;
        error.message = "OBJ v1 cannot losslessly express the current mesh";
        error.affectedGroups = report.losses.front().affectedGroups;
        return Result<ConversionReport>(std::move(error));
    }
    report.notes.push_back("v1 export uses vertex-bound UV/normal indices and one material per group");
    return Result<ConversionReport>(std::move(report));
}

} // namespace meshedit
