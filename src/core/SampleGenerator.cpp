#include "core/SampleGenerator.h"

#include "core/Format.h"

#include <cmath>
#include <filesystem>
#include <fstream>
#include <locale>
#include <set>
#include <sstream>
#include <vector>

namespace meshedit {
namespace {

Face tri(int a, int b, int c, const std::string& group,
         const std::string& material = "default") {
    Face face;
    face.corners = {Corner{a, -1, -1}, Corner{b, -1, -1}, Corner{c, -1, -1}};
    face.group = group;
    face.material = material;
    return face;
}

Mesh makeCube() {
    Mesh mesh;
    mesh.name = "unit_cube";
    mesh.positions = {
        {0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {0, 1, 0},
        {0, 0, 1}, {1, 0, 1}, {1, 1, 1}, {0, 1, 1}};
    mesh.texcoords = {{0, 0}, {1, 0}, {1, 1}, {0, 1}};
    mesh.groups = {"bottom", "top", "sides"};
    mesh.materialLibraries = {"cube.mtl"};

    struct TriangleDef { int a, b, c; int uv[3]; const char* group; const char* material; };
    const TriangleDef defs[] = {
        {0, 2, 1, {0, 2, 1}, "bottom", "rubber"},
        {0, 3, 2, {0, 3, 2}, "bottom", "rubber"},
        {4, 5, 6, {0, 2, 1}, "top", "metal"},
        {4, 6, 7, {0, 3, 2}, "top", "metal"},
        {0, 1, 5, {0, 1, 2}, "sides", "paint"},
        {0, 5, 4, {0, 2, 3}, "sides", "paint"},
        {3, 7, 6, {0, 1, 2}, "sides", "paint"},
        {3, 6, 2, {0, 2, 3}, "sides", "paint"},
        {0, 4, 7, {0, 1, 2}, "sides", "paint"},
        {0, 7, 3, {0, 2, 3}, "sides", "paint"},
        {1, 2, 6, {0, 1, 2}, "sides", "paint"},
        {1, 6, 5, {0, 2, 3}, "sides", "paint"}};
    for (const TriangleDef& def : defs) {
        Face face;
        face.corners = {
            Corner{def.a, def.uv[0], -1},
            Corner{def.b, def.uv[1], -1},
            Corner{def.c, def.uv[2], -1}};
        face.group = def.group;
        face.material = def.material;
        mesh.faces.push_back(face);
    }
    recomputeNormals(mesh);
    return mesh;
}

Mesh makeNonManifoldDegenerate() {
    Mesh mesh;
    mesh.name = "nonmanifold_degenerate";
    mesh.positions = {
        {0, 0, 0}, {1, 0, 0}, {0.5, 1, 0}, {0.5, -1, 0},
        {0.5, 0, 1}, {0.5, 1, 0}, {0.5, 0.5, 0.5}};
    mesh.texcoords = {{0, 0}, {1, 0}, {0.5, 1}};
    mesh.groups = {"fan", "degenerate"};
    mesh.faces.push_back(tri(0, 1, 2, "fan", "steel"));
    mesh.faces.push_back(tri(0, 1, 3, "fan", "steel"));
    mesh.faces.push_back(tri(0, 1, 4, "fan", "steel"));
    mesh.faces.push_back(tri(5, 5, 6, "degenerate", "scrap"));
    for (Face& face : mesh.faces) {
        for (Corner& corner : face.corners) corner.texcoord = 0;
    }
    recomputeNormals(mesh);
    return mesh;
}

Mesh makeSeamVersionLoss() {
    Mesh mesh;
    mesh.name = "seam_version_loss";
    mesh.positions = {{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, 0, 1}};
    mesh.texcoords = {{0, 0}, {1, 0}, {0, 1}, {0.5, 0.5}};
    mesh.groups = {"seam"};
    mesh.faces.push_back(Face{{Corner{0, 0, -1}, Corner{1, 1, -1}, Corner{2, 2, -1}},
                              "seam", "red"});
    mesh.faces.push_back(Face{{Corner{0, 3, -1}, Corner{1, 1, -1}, Corner{3, 2, -1}},
                              "seam", "blue"});
    recomputeNormals(mesh);
    return mesh;
}

Mesh makeLegacyV1() {
    Mesh mesh;
    mesh.name = "legacy_v1";
    mesh.sourceVersion = 1;
    mesh.positions = {{0, 0, 0}, {1, 0, 0}, {0, 1, 0}};
    mesh.texcoords = {{0, 0}, {1, 0}, {0, 1}};
    mesh.groups = {"legacy"};
    mesh.faces.push_back(Face{{Corner{0, 0, -1}, Corner{1, 1, -1}, Corner{2, 2, -1}},
                              "legacy", "legacy_material"});
    recomputeNormals(mesh);
    return mesh;
}

Mesh makeLargeGrid(int columns, int rows) {
    Mesh mesh;
    mesh.name = "large_grid";
    mesh.positions.reserve(static_cast<std::size_t>((columns + 1) * (rows + 1)));
    mesh.texcoords.reserve(static_cast<std::size_t>((columns + 1) * (rows + 1)));
    for (int y = 0; y <= rows; ++y) {
        for (int x = 0; x <= columns; ++x) {
            const double fx = static_cast<double>(x) / columns;
            const double fy = static_cast<double>(y) / rows;
            const double z = 0.08 * std::sin(fx * 9.0) * std::cos(fy * 7.0);
            mesh.positions.push_back({fx, fy, z});
            mesh.texcoords.push_back({fx, fy});
        }
    }
    mesh.groups = {"grid_0", "grid_1", "grid_2", "grid_3"};
    for (int y = 0; y < rows; ++y) {
        for (int x = 0; x < columns; ++x) {
            const int i0 = y * (columns + 1) + x;
            const int i1 = i0 + 1;
            const int i2 = i0 + columns + 2;
            const int i3 = i0 + columns + 1;
            const int band = std::min(3, (x * 4) / columns);
            const std::string group = "grid_" + std::to_string(band);
            Face f1;
            f1.corners = {Corner{i0, i0, -1}, Corner{i1, i1, -1}, Corner{i2, i2, -1}};
            f1.group = group;
            f1.material = "grid_material";
            Face f2;
            f2.corners = {Corner{i0, i0, -1}, Corner{i2, i2, -1}, Corner{i3, i3, -1}};
            f2.group = group;
            f2.material = "grid_material";
            mesh.faces.push_back(f1);
            mesh.faces.push_back(f2);
        }
    }
    recomputeNormals(mesh);
    return mesh;
}

Result<bool> writeAsciiDegenerateStl(const std::string& path) {
    std::ofstream out(path, std::ios::binary | std::ios::trunc);
    if (!out) return Error{Code::IoError, "cannot create degenerate STL sample", 0, {}};
    out << "solid degenerate_sample\n"
        << "  facet normal 0 0 1\n"
        << "    outer loop\n"
        << "      vertex 0 0 0\n"
        << "      vertex 0 0 0\n"
        << "      vertex 1 0 0\n"
        << "    endloop\n"
        << "  endfacet\n"
        << "  facet normal 0 0 1\n"
        << "    outer loop\n"
        << "      vertex 0 0 0\n"
        << "      vertex 1 0 0\n"
        << "      vertex 0 1 0\n"
        << "    endloop\n"
        << "  endfacet\n"
        << "endsolid degenerate_sample\n";
    if (!out) return Error{Code::IoError, "failed writing degenerate STL sample", 0, {}};
    return success();
}

} // namespace

std::string SampleManifest::toJson() const {
    std::ostringstream out;
    out << "{\"directory\":\"" << directory << "\",\"files\":" << fileCount
        << ",\"bytes\":" << totalBytes << '}';
    return out.str();
}

Result<SampleManifest> generateDeterministicSamples(const std::string& outputDirectory) {
    std::error_code error;
    std::filesystem::create_directories(outputDirectory, error);
    if (error) {
        return Error{Code::IoError,
                     "cannot create sample directory: " + error.message(), 0, {}};
    }

    const Mesh cube = makeCube();
    const Mesh nonManifold = makeNonManifoldDegenerate();
    const Mesh seam = makeSeamVersionLoss();
    const Mesh legacy = makeLegacyV1();
    const Mesh grid = makeLargeGrid(64, 64);

    struct Output {
        std::string name;
        Result<ExportResult> result;
    };
    std::vector<Output> outputs;
    outputs.push_back({"cube_v2.obj",
                       writeObj(cube, outputDirectory + "/cube_v2.obj", Version::V2)});
    outputs.push_back({"nonmanifold_degenerate.obj",
                       writeObj(nonManifold, outputDirectory + "/nonmanifold_degenerate.obj",
                                Version::V2)});
    outputs.push_back({"seam_version_loss.obj",
                       writeObj(seam, outputDirectory + "/seam_version_loss.obj",
                                Version::V2)});
    outputs.push_back({"legacy_v1.obj",
                       writeObj(legacy, outputDirectory + "/legacy_v1.obj", Version::V1)});
    outputs.push_back({"large_grid.obj",
                       writeObj(grid, outputDirectory + "/large_grid.obj", Version::V2)});
    outputs.push_back({"cube_ascii.stl",
                       writeStl(cube, outputDirectory + "/cube_ascii.stl",
                                StlVariant::Ascii)});
    outputs.push_back({"cube_binary.stl",
                       writeStl(cube, outputDirectory + "/cube_binary.stl",
                                StlVariant::Binary)});
    outputs.push_back({"nonmanifold_degenerate.stl",
                       writeStl(nonManifold,
                                outputDirectory + "/nonmanifold_degenerate.stl",
                                StlVariant::Binary)});
    for (const Output& output : outputs) {
        if (!output.result.ok()) {
            return Error{output.result.error.code,
                         output.name + ": " + output.result.error.message,
                         output.result.error.line, output.result.error.affectedGroups};
        }
    }
    Result<bool> degenerate =
        writeAsciiDegenerateStl(outputDirectory + "/degenerate_ascii.stl");
    if (!degenerate.ok()) return degenerate.error;

    SampleManifest manifest;
    manifest.directory = "samples";
    std::vector<std::string> generatedNames;
    for (const Output& output : outputs) generatedNames.push_back(output.name);
    generatedNames.push_back("degenerate_ascii.stl");
    for (const std::string& name : generatedNames) {
        const std::filesystem::path file = std::filesystem::path(outputDirectory) / name;
        if (!std::filesystem::is_regular_file(file)) {
            return Error{Code::IoError, "generated sample is missing: " + name, 0, {}};
        }
        ++manifest.fileCount;
        manifest.totalBytes += static_cast<long long>(std::filesystem::file_size(file));
    }
    return Result<SampleManifest>(std::move(manifest));
}

} // namespace meshedit
