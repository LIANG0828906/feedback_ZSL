#pragma once

#include "font_atlas/json.hpp"
#include "font_atlas/types.hpp"

#include <map>
#include <string>
#include <vector>

namespace font_atlas {

struct FontLoadOptions {
    double curveTolerance = 0.2;
};

struct NormalizedGlyph {
    std::string name;
    std::vector<std::uint32_t> unicodes;
    double advance = 0.0;
    BoundsD bounds;
    std::vector<std::vector<PointD>> contours;
};

struct Font {
    int version = 1;
    std::string name;
    double unitsPerEm = 1000.0;
    double ascent = 0.0;
    double descent = 0.0;
    std::string fingerprint;
    std::vector<std::string> glyphOrder;
    std::map<std::string, NormalizedGlyph> glyphs;
    std::map<std::uint32_t, std::string> unicodeToGlyph;

    const NormalizedGlyph* findGlyph(const std::string& glyphName) const;
    const NormalizedGlyph* findUnicode(std::uint32_t codePoint) const;
};

Result<Font> loadFontFile(const std::string& path, const FontLoadOptions& options = {});
Result<Font> parseFont(const Json& root, const FontLoadOptions& options = {});

double signedArea(const std::vector<PointD>& contour);
std::string normalizedGlyphFingerprint(const NormalizedGlyph& glyph);
std::string fontFingerprint(const Font& font);

}  // namespace font_atlas

