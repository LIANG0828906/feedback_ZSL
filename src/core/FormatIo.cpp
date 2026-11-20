#include "core/Format.h"

#include "core/VersionCompat.h"

#include <algorithm>
#include <cctype>
#include <fstream>
#include <locale>
#include <sstream>

namespace meshedit {
namespace {

std::string lower(std::string text) {
    std::transform(text.begin(), text.end(), text.begin(),
                   [](unsigned char ch) { return static_cast<char>(std::tolower(ch)); });
    return text;
}

std::string jsonEscape(const std::string& text) {
    std::string result;
    for (unsigned char ch : text) {
        switch (ch) {
        case '"': result += "\\\""; break;
        case '\\': result += "\\\\"; break;
        case '\n': result += "\\n"; break;
        case '\r': result += "\\r"; break;
        case '\t': result += "\\t"; break;
        default:
            if (ch < 0x20) {
                const char hex[] = "0123456789abcdef";
                result += "\\u00";
                result.push_back(hex[(ch >> 4) & 0xf]);
                result.push_back(hex[ch & 0xf]);
            } else {
                result.push_back(static_cast<char>(ch));
            }
        }
    }
    return result;
}

} // namespace

MeshFormat formatFromPath(const std::string& path) {
    const std::string extension = lower(path.substr(path.find_last_of(".") + 1));
    if (extension == "stl") return MeshFormat::Stl;
    return MeshFormat::Obj;
}

const char* formatName(MeshFormat format) {
    return format == MeshFormat::Obj ? "obj" : "stl";
}

int versionNumber(Version version) {
    return version == Version::V1 ? 1 : 2;
}

std::string ConversionReport::toText() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "conversion " << formatName(sourceFormat) << " v" << sourceVersion
        << " -> " << formatName(targetFormat) << " v" << targetVersion << '\n';
    if (upgradedOnLoad) out << "automatic upgrade: v1 -> v2\n";
    for (const std::string& note : notes) out << "note: " << note << '\n';
    for (const LossItem& loss : losses) {
        out << codeName(loss.code) << ": " << loss.message;
        if (!loss.affectedGroups.empty()) {
            out << " [groups:";
            for (const std::string& group : loss.affectedGroups) out << ' ' << group;
            out << ']';
        }
        if (loss.line > 0) out << " [line " << loss.line << ']';
        out << '\n';
    }
    if (losses.empty()) out << "no attributed loss\n";
    return out.str();
}

std::string ConversionReport::toJson() const {
    std::ostringstream out;
    out.imbue(std::locale::classic());
    out << "{\"sourceFormat\":\"" << formatName(sourceFormat)
        << "\",\"targetFormat\":\"" << formatName(targetFormat)
        << "\",\"sourceVersion\":" << sourceVersion
        << ",\"targetVersion\":" << targetVersion
        << ",\"upgradedOnLoad\":" << (upgradedOnLoad ? "true" : "false")
        << ",\"notes\":[";
    for (std::size_t i = 0; i < notes.size(); ++i) {
        if (i) out << ',';
        out << '"' << jsonEscape(notes[i]) << '"';
    }
    out << "],\"losses\":[";
    for (std::size_t i = 0; i < losses.size(); ++i) {
        if (i) out << ',';
        const LossItem& loss = losses[i];
        out << "{\"code\":\"" << codeName(loss.code)
            << "\",\"message\":\"" << jsonEscape(loss.message)
            << "\",\"line\":" << loss.line << ",\"groups\":[";
        for (std::size_t j = 0; j < loss.affectedGroups.size(); ++j) {
            if (j) out << ',';
            out << '"' << jsonEscape(loss.affectedGroups[j]) << '"';
        }
        out << "]}";
    }
    out << "]}";
    return out.str();
}

Result<LoadResult> loadMesh(const std::string& path, const LoadOptions& options) {
    const MeshFormat format = formatFromPath(path);
    Result<LoadResult> loaded = format == MeshFormat::Stl
        ? parseStl(path, options.stlVariant)
        : parseObj(path);
    if (!loaded.ok()) return loaded;
    const int sourceVersion = loaded->mesh.sourceVersion;
    ConversionReport parserReport = std::move(loaded->report);
    Result<ConversionReport> compatibility =
        upgradeLoadedMesh(loaded->mesh, format, sourceVersion);
    if (!compatibility.ok()) return compatibility.error;
    ConversionReport merged = std::move(compatibility.get());
    merged.upgradedOnLoad = merged.upgradedOnLoad || parserReport.upgradedOnLoad;
    merged.notes.insert(merged.notes.end(), parserReport.notes.begin(),
                        parserReport.notes.end());
    merged.losses.insert(merged.losses.end(), parserReport.losses.begin(),
                         parserReport.losses.end());
    loaded->report = std::move(merged);
    return loaded;
}

Result<ExportResult> exportMesh(const Mesh& mesh, const std::string& path,
                                const ExportOptions& options) {
    Result<ConversionReport> compatibility = checkExportCompatibility(mesh, options);
    if (!compatibility.ok()) return compatibility.error;
    if (options.format == MeshFormat::Stl) {
        Result<ExportResult> result = writeStl(mesh, path, options.stlVariant);
        if (!result.ok()) return result;
        ConversionReport report = std::move(compatibility.get());
        report.losses = std::move(result->report.losses);
        report.notes.insert(report.notes.end(),
                            result->report.notes.begin(), result->report.notes.end());
        result->report = std::move(report);
        return result;
    }
    Result<ExportResult> result = writeObj(mesh, path, options.version);
    if (!result.ok()) return result;
    result->report.losses = compatibility->losses;
    result->report.notes.insert(result->report.notes.end(),
                                compatibility->notes.begin(), compatibility->notes.end());
    return result;
}

} // namespace meshedit
