#include "font_atlas/font.hpp"

#include "font_atlas/sha256.hpp"

#include <algorithm>
#include <cmath>
#include <exception>
#include <iomanip>
#include <limits>
#include <set>
#include <sstream>

namespace font_atlas {
namespace {

struct Affine {
    double a = 1.0;
    double b = 0.0;
    double c = 0.0;
    double d = 1.0;
    double e = 0.0;
    double f = 0.0;

    PointD apply(const PointD& point) const {
        return {a * point.x + c * point.y + e, b * point.x + d * point.y + f};
    }

    Affine then(const Affine& outer) const {
        Affine result;
        result.a = outer.a * a + outer.c * b;
        result.b = outer.b * a + outer.d * b;
        result.c = outer.a * c + outer.c * d;
        result.d = outer.b * c + outer.d * d;
        result.e = outer.a * e + outer.c * f + outer.e;
        result.f = outer.b * e + outer.d * f + outer.f;
        return result;
    }

    double determinant() const { return a * d - b * c; }
};

enum class CurveType {
    Line,
    Quadratic,
    Cubic,
    Close
};

struct RawSegment {
    CurveType type = CurveType::Line;
    PointD control1;
    PointD control2;
    PointD to;
    int sourceIndex = -1;
};

struct RawContour {
    PointD start;
    bool hasStart = false;
    std::vector<RawSegment> segments;
    std::string direction;
};

struct RawGlyph {
    std::string name;
    std::vector<std::uint32_t> unicodes;
    double advance = 0.0;
    std::vector<RawContour> contours;
    struct Component {
        std::string glyph;
        Affine transform;
        int sourceIndex = -1;
    };
    std::vector<Component> components;
};

Diagnostic invalidFont(const std::string& glyph, int contour, int segment,
                       const std::string& message) {
    Diagnostic diagnostic;
    diagnostic.code = ErrorCode::FontInvalid;
    diagnostic.glyph = glyph;
    diagnostic.contourIndex = contour;
    diagnostic.segmentIndex = segment;
    diagnostic.message = message;
    return diagnostic;
}

bool finitePoint(const PointD& point) {
    return std::isfinite(point.x) && std::isfinite(point.y);
}

Result<PointD> parsePoint(const Json& value, const std::string& glyph, int contour, int segment,
                          const std::string& field) {
    auto error = [&](const std::string& message) {
        return Result<PointD>::failure(invalidFont(
            glyph, contour, segment, "missing or invalid " + field + ": " + message));
    };
    PointD point;
    if (value.isObject()) {
        const Json* x = value.find("x");
        const Json* y = value.find("y");
        if (x == nullptr || y == nullptr || !x->isNumber() || !y->isNumber()) {
            return error("expected object with numeric x and y");
        }
        point.x = x->asNumber();
        point.y = y->asNumber();
    } else if (value.isArray()) {
        const auto& items = value.asArray();
        if (items.size() != 2 || !items[0].isNumber() || !items[1].isNumber()) {
            return error("expected [x, y]");
        }
        point.x = items[0].asNumber();
        point.y = items[1].asNumber();
    } else {
        return error("expected point object or pair");
    }
    if (!finitePoint(point)) {
        return error("coordinates must be finite");
    }
    return Result<PointD>::success(point);
}

Result<PointD> pointReference(const Json& segment, const Json& points, const char* field,
                              const std::string& glyph, int contour, int segmentIndex) {
    const Json* value = segment.find(field);
    if (value == nullptr) {
        return Result<PointD>::failure(invalidFont(
            glyph, contour, segmentIndex, std::string("missing control point '") + field + "'"));
    }
    if (value->isNumber()) {
        if (!points.isArray()) {
            return Result<PointD>::failure(
                invalidFont(glyph, contour, segmentIndex,
                            std::string("point index '") + field + "' requires a points array"));
        }
        int index = -1;
        try {
            index = value->asInt();
        } catch (const std::exception&) {
            return Result<PointD>::failure(
                invalidFont(glyph, contour, segmentIndex,
                            std::string("point index '") + field +
                                "' must be an integer"));
        }
        if (index < 0 || static_cast<std::size_t>(index) >= points.size()) {
            return Result<PointD>::failure(
                invalidFont(glyph, contour, segmentIndex,
                            std::string("point index '") + field + "' is out of range"));
        }
        return parsePoint(points.asArray()[static_cast<std::size_t>(index)], glyph, contour,
                          segmentIndex, field);
    }
    return parsePoint(*value, glyph, contour, segmentIndex, field);
}

Result<std::uint32_t> parseUnicodeValue(const Json& value, const std::string& glyph) {
    auto failure = [&](const std::string& message) {
        return Result<std::uint32_t>::failure(invalidFont(glyph, -1, -1, message));
    };
    if (value.isNumber()) {
        std::int64_t code = 0;
        try {
            code = value.asInt64();
        } catch (const std::exception&) {
            return failure("Unicode mapping must be an integer code point");
        }
        if (code < 0 || code > 0x10ffff) {
            return failure("Unicode code point out of range");
        }
        if (code >= 0xd800 && code <= 0xdfff) {
            return failure("Unicode surrogate code points are not valid mappings");
        }
        return Result<std::uint32_t>::success(static_cast<std::uint32_t>(code));
    }
    if (!value.isString()) {
        return failure("Unicode mapping must be a number or U+XXXX string");
    }
    const std::string& text = value.asString();
    if (text.size() < 3 || (text[0] != 'U' && text[0] != 'u') || text[1] != '+') {
        return failure("Unicode mapping string must use U+XXXX syntax");
    }
    std::uint32_t code = 0;
    for (std::size_t i = 2; i < text.size(); ++i) {
        const char c = text[i];
        int digit = -1;
        if (c >= '0' && c <= '9') {
            digit = c - '0';
        } else if (c >= 'a' && c <= 'f') {
            digit = 10 + c - 'a';
        } else if (c >= 'A' && c <= 'F') {
            digit = 10 + c - 'A';
        }
        if (digit < 0) {
            return failure("invalid Unicode hexadecimal digit");
        }
        code = (code << 4u) | static_cast<std::uint32_t>(digit);
    }
    if (code > 0x10ffff) {
        return failure("Unicode code point out of range");
    }
    if (code >= 0xd800u && code <= 0xdfffu) {
        return failure("Unicode surrogate code points are not valid mappings");
    }
    return Result<std::uint32_t>::success(code);
}

Result<RawGlyph> parseRawGlyph(const Json& value, const std::string& fallbackName) {
    std::string diagnosticGlyph = fallbackName;
    auto failure = [&](int contour, int segment, const std::string& message) {
        return Result<RawGlyph>::failure(
            invalidFont(diagnosticGlyph, contour, segment, message));
    };
    if (!value.isObject()) {
        return failure(-1, -1, "glyph must be an object");
    }
    RawGlyph glyph;
    const Json* name = value.find("name");
    if (name == nullptr) {
        glyph.name = fallbackName;
    } else if (!name->isString() || name->asString().empty()) {
        return failure(-1, -1, "glyph name must be a non-empty string");
    } else {
        glyph.name = name->asString();
    }
    diagnosticGlyph = glyph.name;

    const Json* advance = value.find("advance");
    if (advance != nullptr) {
        if (!advance->isNumber() || !std::isfinite(advance->asNumber()) ||
            advance->asNumber() < 0.0) {
            return failure(-1, -1, "advance must be a finite non-negative number");
        }
        glyph.advance = advance->asNumber();
    }

    if (const Json* unicode = value.find("unicode")) {
        auto parsed = parseUnicodeValue(*unicode, glyph.name);
        if (!parsed.ok()) {
            return Result<RawGlyph>::failure(parsed.error);
        }
        glyph.unicodes.push_back(*parsed.value);
    }
    if (const Json* unicodes = value.find("unicodes")) {
        if (!unicodes->isArray()) {
            return failure(-1, -1, "unicodes must be an array");
        }
        for (const auto& item : unicodes->asArray()) {
            auto parsed = parseUnicodeValue(item, glyph.name);
            if (!parsed.ok()) {
                return Result<RawGlyph>::failure(parsed.error);
            }
            glyph.unicodes.push_back(*parsed.value);
        }
    }

    const Json* contours = value.find("contours");
    if (contours != nullptr) {
        if (!contours->isArray()) {
            return failure(-1, -1, "contours must be an array");
        }
        int contourIndex = 0;
        for (const auto& contourValue : contours->asArray()) {
            if (!contourValue.isObject()) {
                return failure(contourIndex, -1, "contour must be an object");
            }
            RawContour contour;
            if (const Json* direction = contourValue.find("direction")) {
                if (!direction->isString()) {
                    return failure(contourIndex, -1, "direction must be a string");
                }
                contour.direction = direction->asString();
                if (contour.direction != "cw" && contour.direction != "ccw" &&
                    contour.direction != "clockwise" &&
                    contour.direction != "counterclockwise") {
                    return failure(contourIndex, -1, "direction must be cw/ccw");
                }
            }

            Json points;
            if (const Json* pointValue = contourValue.find("points")) {
                if (!pointValue->isArray()) {
                    return failure(contourIndex, -1, "points must be an array");
                }
                points = *pointValue;
                if (!points.asArray().empty()) {
                    auto start = parsePoint(points.asArray()[0], glyph.name, contourIndex, -1,
                                            "points[0]");
                    if (!start.ok()) {
                        return Result<RawGlyph>::failure(start.error);
                    }
                    contour.start = *start.value;
                    contour.hasStart = true;
                }
            }
            if (const Json* start = contourValue.find("start")) {
                auto parsed = parsePoint(*start, glyph.name, contourIndex, -1, "start");
                if (!parsed.ok()) {
                    return Result<RawGlyph>::failure(parsed.error);
                }
                contour.start = *parsed.value;
                contour.hasStart = true;
            }
            PointD current = contour.start;
            bool hasCurrent = contour.hasStart;

            const Json* segments = contourValue.find("segments");
            if (segments != nullptr) {
                if (!segments->isArray()) {
                    return failure(contourIndex, -1, "segments must be an array");
                }
                int segmentIndex = 0;
                for (const auto& segmentValue : segments->asArray()) {
                    if (!segmentValue.isObject()) {
                        return failure(contourIndex, segmentIndex, "segment must be an object");
                    }
                    const Json* typeValue = segmentValue.find("type");
                    if (typeValue == nullptr || !typeValue->isString()) {
                        return failure(contourIndex, segmentIndex,
                                       "missing or invalid segment type");
                    }
                    const std::string type = typeValue->asString();
                    RawSegment segment;
                    segment.sourceIndex = segmentIndex;
                    if (type == "line" || type == "L" || type == "l") {
                        segment.type = CurveType::Line;
                    } else if (type == "quad" || type == "quadratic" || type == "Q" ||
                               type == "q") {
                        segment.type = CurveType::Quadratic;
                    } else if (type == "cubic" || type == "C" || type == "c") {
                        segment.type = CurveType::Cubic;
                    } else if (type == "close" || type == "Z" || type == "z") {
                        segment.type = CurveType::Close;
                    } else {
                        return failure(contourIndex, segmentIndex,
                                       "illegal segment type '" + type + "'");
                    }

                    if (segment.type == CurveType::Close) {
                        if (segmentValue.contains("to") || segmentValue.contains("control") ||
                            segmentValue.contains("control1") ||
                            segmentValue.contains("control2")) {
                            return failure(contourIndex, segmentIndex,
                                           "close segment must not carry control points");
                        }
                        contour.segments.push_back(segment);
                        ++segmentIndex;
                        continue;
                    }

                    if (const Json* from = segmentValue.find("from")) {
                        auto parsed =
                            pointReference(segmentValue, points, "from", glyph.name,
                                           contourIndex, segmentIndex);
                        if (!parsed.ok()) {
                            return Result<RawGlyph>::failure(parsed.error);
                        }
                        if (hasCurrent &&
                            (std::abs(current.x - parsed.value->x) > 1e-9 ||
                             std::abs(current.y - parsed.value->y) > 1e-9)) {
                            return failure(contourIndex, segmentIndex,
                                           "segment 'from' does not match contour endpoint");
                        }
                        if (!hasCurrent) {
                            contour.start = *parsed.value;
                            contour.hasStart = true;
                        }
                        current = *parsed.value;
                        hasCurrent = true;
                        (void)from;
                    }
                    if (!hasCurrent) {
                        return failure(contourIndex, segmentIndex,
                                       "first segment requires a start point or 'from'");
                    }

                    auto to = pointReference(segmentValue, points, "to", glyph.name,
                                             contourIndex, segmentIndex);
                    if (!to.ok()) {
                        return Result<RawGlyph>::failure(to.error);
                    }
                    segment.to = *to.value;
                    current = segment.to;
                    hasCurrent = true;
                    if (segment.type == CurveType::Quadratic) {
                        auto control = pointReference(segmentValue, points, "control",
                                                      glyph.name, contourIndex, segmentIndex);
                        if (!control.ok()) {
                            return Result<RawGlyph>::failure(control.error);
                        }
                        segment.control1 = *control.value;
                    } else if (segment.type == CurveType::Cubic) {
                        auto control1 = pointReference(segmentValue, points, "control1",
                                                       glyph.name, contourIndex, segmentIndex);
                        if (!control1.ok()) {
                            return Result<RawGlyph>::failure(control1.error);
                        }
                        auto control2 = pointReference(segmentValue, points, "control2",
                                                       glyph.name, contourIndex, segmentIndex);
                        if (!control2.ok()) {
                            return Result<RawGlyph>::failure(control2.error);
                        }
                        segment.control1 = *control1.value;
                        segment.control2 = *control2.value;
                    }
                    contour.segments.push_back(segment);
                    ++segmentIndex;
                }
            } else if (points.isArray() && points.size() >= 3) {
                for (std::size_t i = 1; i < points.size(); ++i) {
                    RawSegment segment;
                    segment.type = CurveType::Line;
                    segment.sourceIndex = static_cast<int>(i - 1);
                    auto to = parsePoint(points.asArray()[i], glyph.name, contourIndex,
                                         static_cast<int>(i - 1), "point");
                    if (!to.ok()) {
                        return Result<RawGlyph>::failure(to.error);
                    }
                    segment.to = *to.value;
                    contour.segments.push_back(segment);
                }
            }
            glyph.contours.push_back(std::move(contour));
            ++contourIndex;
        }
    }

    if (const Json* components = value.find("components")) {
        if (!components->isArray()) {
            return failure(-1, -1, "components must be an array");
        }
        int componentIndex = 0;
        for (const auto& componentValue : components->asArray()) {
            if (!componentValue.isObject()) {
                return failure(-1, componentIndex, "component must be an object");
            }
            const Json* reference = componentValue.find("glyph");
            if (reference == nullptr || !reference->isString() ||
                reference->asString().empty()) {
                return failure(-1, componentIndex,
                               "component requires a non-empty glyph reference");
            }
            RawGlyph::Component component;
            component.glyph = reference->asString();
            component.sourceIndex = componentIndex;
            if (const Json* dx = componentValue.find("dx")) {
                if (!dx->isNumber() || !std::isfinite(dx->asNumber())) {
                    return failure(-1, componentIndex,
                                   "component dx must be a finite number");
                }
                component.transform.e = dx->asNumber();
            }
            if (const Json* dy = componentValue.find("dy")) {
                if (!dy->isNumber() || !std::isfinite(dy->asNumber())) {
                    return failure(-1, componentIndex,
                                   "component dy must be a finite number");
                }
                component.transform.f = dy->asNumber();
            }
            double scaleX = 1.0;
            double scaleY = 1.0;
            if (const Json* scale = componentValue.find("scale")) {
                if (!scale->isNumber()) {
                    return failure(-1, componentIndex, "component scale must be a number");
                }
                scaleX = scale->asNumber();
                scaleY = scale->asNumber();
            }
            if (const Json* sx = componentValue.find("scaleX")) {
                if (!sx->isNumber()) {
                    return failure(-1, componentIndex, "component scaleX must be a number");
                }
                scaleX = sx->asNumber();
            }
            if (const Json* sy = componentValue.find("scaleY")) {
                if (!sy->isNumber()) {
                    return failure(-1, componentIndex, "component scaleY must be a number");
                }
                scaleY = sy->asNumber();
            }
            component.transform.a = scaleX;
            component.transform.d = scaleY;
            if (!std::isfinite(component.transform.e) ||
                !std::isfinite(component.transform.f) || !std::isfinite(scaleX) ||
                !std::isfinite(scaleY) || scaleX == 0.0 || scaleY == 0.0) {
                return failure(-1, componentIndex, "component transform is not finite/non-zero");
            }
            if (const Json* matrix = componentValue.find("matrix")) {
                if (!matrix->isArray() || matrix->size() != 6) {
                    return failure(-1, componentIndex,
                                   "component matrix must contain six numbers");
                }
                double values[6] = {};
                for (std::size_t i = 0; i < 6; ++i) {
                    if (!matrix->asArray()[i].isNumber()) {
                        return failure(-1, componentIndex,
                                       "component matrix values must be numbers");
                    }
                    values[i] = matrix->asArray()[i].asNumber();
                }
                component.transform.a = values[0];
                component.transform.b = values[1];
                component.transform.c = values[2];
                component.transform.d = values[3];
                component.transform.e = values[4];
                component.transform.f = values[5];
                for (double value : values) {
                    if (!std::isfinite(value)) {
                        return failure(-1, componentIndex,
                                       "component matrix values must be finite");
                    }
                }
                if (std::abs(component.transform.determinant()) < 1e-12) {
                    return failure(-1, componentIndex,
                                   "component matrix must be invertible");
                }
            }
            glyph.components.push_back(std::move(component));
            ++componentIndex;
        }
    }
    return Result<RawGlyph>::success(std::move(glyph));
}

void flattenQuadratic(const PointD& p0, const PointD& p1, const PointD& p2, double tolerance,
                      int depth, std::vector<PointD>& output) {
    const double dx = p0.x - 2.0 * p1.x + p2.x;
    const double dy = p0.y - 2.0 * p1.y + p2.y;
    if (depth >= 16 || std::sqrt(dx * dx + dy * dy) <= tolerance * 4.0) {
        output.push_back(p2);
        return;
    }
    const PointD a{(p0.x + p1.x) * 0.5, (p0.y + p1.y) * 0.5};
    const PointD b{(p1.x + p2.x) * 0.5, (p1.y + p2.y) * 0.5};
    const PointD mid{(a.x + b.x) * 0.5, (a.y + b.y) * 0.5};
    flattenQuadratic(p0, a, mid, tolerance, depth + 1, output);
    flattenQuadratic(mid, b, p2, tolerance, depth + 1, output);
}

void flattenCubic(const PointD& p0, const PointD& p1, const PointD& p2, const PointD& p3,
                  double tolerance, int depth, std::vector<PointD>& output) {
    const PointD d1{p0.x - 2.0 * p1.x + p2.x, p0.y - 2.0 * p1.y + p2.y};
    const PointD d2{p1.x - 2.0 * p2.x + p3.x, p1.y - 2.0 * p2.y + p3.y};
    const double maxDeviation =
        std::max(std::sqrt(d1.x * d1.x + d1.y * d1.y),
                 std::sqrt(d2.x * d2.x + d2.y * d2.y));
    if (depth >= 18 || maxDeviation <= tolerance * 8.0) {
        output.push_back(p3);
        return;
    }
    const PointD p01{(p0.x + p1.x) * 0.5, (p0.y + p1.y) * 0.5};
    const PointD p12{(p1.x + p2.x) * 0.5, (p1.y + p2.y) * 0.5};
    const PointD p23{(p2.x + p3.x) * 0.5, (p2.y + p3.y) * 0.5};
    const PointD p012{(p01.x + p12.x) * 0.5, (p01.y + p12.y) * 0.5};
    const PointD p123{(p12.x + p23.x) * 0.5, (p12.y + p23.y) * 0.5};
    const PointD mid{(p012.x + p123.x) * 0.5, (p012.y + p123.y) * 0.5};
    flattenCubic(p0, p01, p012, mid, tolerance, depth + 1, output);
    flattenCubic(mid, p123, p23, p3, tolerance, depth + 1, output);
}

void expandRawContours(const std::map<std::string, RawGlyph>& rawGlyphs,
                       const std::string& glyphName, const Affine& transform,
                       std::vector<std::string>& stack, double tolerance,
                       std::vector<std::vector<PointD>>& output, Diagnostic& error) {
    const auto found = rawGlyphs.find(glyphName);
    if (found == rawGlyphs.end()) {
        error = invalidFont(stack.empty() ? glyphName : stack.back(), -1, -1,
                            "unknown glyph reference '" + glyphName + "'");
        return;
    }
    if (std::find(stack.begin(), stack.end(), glyphName) != stack.end()) {
        std::string chain;
        for (const auto& name : stack) {
            if (!chain.empty()) {
                chain += " -> ";
            }
            chain += name;
        }
        chain += " -> " + glyphName;
        error = invalidFont(glyphName, -1, -1, "component cycle detected: " + chain);
        return;
    }
    stack.push_back(glyphName);
    const RawGlyph& glyph = found->second;
    for (const auto& component : glyph.components) {
        expandRawContours(rawGlyphs, component.glyph, component.transform.then(transform),
                          stack, tolerance, output, error);
        if (!error.ok()) {
            error.segmentIndex = component.sourceIndex;
            error.glyph = glyphName;
            return;
        }
    }

    for (const auto& rawContour : glyph.contours) {
        if (!rawContour.hasStart) {
            if (rawContour.segments.empty()) {
                continue;
            }
            error = invalidFont(glyphName, -1, -1, "contour has no start point");
            return;
        }
        std::vector<PointD> points;
        PointD current = transform.apply(rawContour.start);
        points.push_back(current);
        bool closedExplicitly = false;
        for (const auto& segment : rawContour.segments) {
            switch (segment.type) {
                case CurveType::Line:
                    current = transform.apply(segment.to);
                    points.push_back(current);
                    break;
                case CurveType::Quadratic:
                    flattenQuadratic(current, transform.apply(segment.control1),
                                     transform.apply(segment.to), tolerance, 0, points);
                    current = transform.apply(segment.to);
                    break;
                case CurveType::Cubic:
                    flattenCubic(current, transform.apply(segment.control1),
                                 transform.apply(segment.control2),
                                 transform.apply(segment.to), tolerance, 0, points);
                    current = transform.apply(segment.to);
                    break;
                case CurveType::Close:
                    closedExplicitly = true;
                    break;
            }
        }
        if (points.size() < 2) {
            stack.pop_back();
            continue;
        }
        if (std::abs(points.back().x - points.front().x) > 1e-12 ||
            std::abs(points.back().y - points.front().y) > 1e-12) {
            points.push_back(points.front());
        }
        if (points.size() < 3) {
            stack.pop_back();
            continue;
        }
        (void)closedExplicitly;
        const double area = signedArea(points);
        if (!rawContour.direction.empty()) {
            const bool wantsCcw = rawContour.direction == "ccw" ||
                                  rawContour.direction == "counterclockwise";
            if ((wantsCcw && area < 0.0) || (!wantsCcw && area > 0.0)) {
                std::reverse(points.begin(), points.end());
            }
        }
        output.push_back(std::move(points));
    }
    stack.pop_back();
}

BoundsD contourBounds(const std::vector<std::vector<PointD>>& contours) {
    BoundsD bounds{std::numeric_limits<double>::infinity(),
                   std::numeric_limits<double>::infinity(),
                   -std::numeric_limits<double>::infinity(),
                   -std::numeric_limits<double>::infinity()};
    for (const auto& contour : contours) {
        for (const auto& point : contour) {
            bounds.x0 = std::min(bounds.x0, point.x);
            bounds.y0 = std::min(bounds.y0, point.y);
            bounds.x1 = std::max(bounds.x1, point.x);
            bounds.y1 = std::max(bounds.y1, point.y);
        }
    }
    if (!std::isfinite(bounds.x0)) {
        bounds = {0.0, 0.0, 0.0, 0.0};
    }
    return bounds;
}

}  // namespace

const NormalizedGlyph* Font::findGlyph(const std::string& glyphName) const {
    const auto iterator = glyphs.find(glyphName);
    return iterator == glyphs.end() ? nullptr : &iterator->second;
}

const NormalizedGlyph* Font::findUnicode(std::uint32_t codePoint) const {
    const auto iterator = unicodeToGlyph.find(codePoint);
    return iterator == unicodeToGlyph.end() ? nullptr : findGlyph(iterator->second);
}

double signedArea(const std::vector<PointD>& contour) {
    double area = 0.0;
    if (contour.size() < 3) {
        return area;
    }
    for (std::size_t i = 0; i < contour.size(); ++i) {
        const PointD& a = contour[i];
        const PointD& b = contour[(i + 1) % contour.size()];
        area += a.x * b.y - b.x * a.y;
    }
    return area * 0.5;
}

std::string normalizedGlyphFingerprint(const NormalizedGlyph& glyph) {
    std::ostringstream stream;
    stream << std::setprecision(17);
    stream << glyph.name << '|' << glyph.advance;
    for (std::uint32_t unicode : glyph.unicodes) {
        stream << '|' << unicode;
    }
    for (const auto& contour : glyph.contours) {
        stream << ";C";
        for (const auto& point : contour) {
            stream << ':' << point.x << ',' << point.y;
        }
    }
    return Sha256::hex(stream.str());
}

std::string fontFingerprint(const Font& font) {
    std::ostringstream stream;
    stream << std::setprecision(17);
    stream << font.version << '|' << font.unitsPerEm << '|' << font.ascent << '|'
           << font.descent;
    for (const auto& glyphName : font.glyphOrder) {
        const auto* glyph = font.findGlyph(glyphName);
        if (glyph != nullptr) {
            stream << '|' << normalizedGlyphFingerprint(*glyph);
        }
    }
    return Sha256::hex(stream.str());
}

Result<Font> parseFont(const Json& root, const FontLoadOptions& options) {
    Diagnostic diagnostic;
    if (!root.isObject()) {
        diagnostic.code = ErrorCode::FontInvalid;
        diagnostic.message = "font root must be a JSON object";
        return Result<Font>::failure(std::move(diagnostic));
    }
    Font font;
    font.version = jsonInt(root, "version", 1, diagnostic);
    font.name = jsonString(root, "name", "unnamed", diagnostic);
    font.unitsPerEm = jsonDouble(root, "unitsPerEm", 1000.0, diagnostic);
    font.ascent = jsonDouble(root, "ascent", font.unitsPerEm, diagnostic);
    font.descent = jsonDouble(root, "descent", 0.0, diagnostic);
    if (!diagnostic.ok()) {
        diagnostic.code = ErrorCode::FontInvalid;
        return Result<Font>::failure(std::move(diagnostic));
    }
    if (font.version < 1) {
        diagnostic.code = ErrorCode::FontInvalid;
        diagnostic.message = "font version must be >= 1";
        return Result<Font>::failure(std::move(diagnostic));
    }
    if (!std::isfinite(font.unitsPerEm) || font.unitsPerEm <= 0.0) {
        diagnostic.code = ErrorCode::FontInvalid;
        diagnostic.message = "unitsPerEm must be a positive finite number";
        return Result<Font>::failure(std::move(diagnostic));
    }
    if (!std::isfinite(options.curveTolerance) || options.curveTolerance <= 0.0) {
        diagnostic.code = ErrorCode::InvalidArgument;
        diagnostic.message = "curve tolerance must be positive";
        return Result<Font>::failure(std::move(diagnostic));
    }

    const Json* glyphs = root.find("glyphs");
    if (glyphs == nullptr || !glyphs->isArray()) {
        diagnostic.code = ErrorCode::FontInvalid;
        diagnostic.message = "font requires a glyphs array";
        return Result<Font>::failure(std::move(diagnostic));
    }

    std::map<std::string, RawGlyph> rawGlyphs;
    for (std::size_t i = 0; i < glyphs->size(); ++i) {
        auto parsed = parseRawGlyph(glyphs->asArray()[i], "glyph" + std::to_string(i));
        if (!parsed.ok()) {
            return Result<Font>::failure(std::move(parsed.error));
        }
        if (rawGlyphs.count(parsed.value->name) != 0) {
            return Result<Font>::failure(
                invalidFont(parsed.value->name, -1, -1, "duplicate glyph name"));
        }
        rawGlyphs.emplace(parsed.value->name, std::move(*parsed.value));
    }
    if (rawGlyphs.empty()) {
        diagnostic.code = ErrorCode::FontInvalid;
        diagnostic.message = "font glyph set is empty";
        return Result<Font>::failure(std::move(diagnostic));
    }

    for (const auto& item : rawGlyphs) {
        const RawGlyph& raw = item.second;
        NormalizedGlyph normalized;
        normalized.name = raw.name;
        normalized.unicodes = raw.unicodes;
        normalized.advance = raw.advance > 0.0 ? raw.advance : font.unitsPerEm * 0.5;
        std::vector<std::string> stack;
        Diagnostic expansionError;
        expandRawContours(rawGlyphs, raw.name, Affine{}, stack, options.curveTolerance,
                          normalized.contours, expansionError);
        if (!expansionError.ok()) {
            return Result<Font>::failure(std::move(expansionError));
        }
        normalized.bounds = contourBounds(normalized.contours);
        if (normalized.contours.empty()) {
            normalized.bounds = {0.0, 0.0, normalized.advance, font.unitsPerEm};
        }
        for (std::uint32_t unicode : normalized.unicodes) {
            const auto existing = font.unicodeToGlyph.find(unicode);
            if (existing != font.unicodeToGlyph.end()) {
                Diagnostic duplicate;
                duplicate.code = ErrorCode::FontInvalid;
                duplicate.glyph = normalized.name;
                duplicate.segmentIndex = -1;
                duplicate.message = "duplicate Unicode mapping U+" +
                                    [&]() {
                                        std::ostringstream text;
                                        text << std::hex << std::uppercase << unicode;
                                        return text.str();
                                    }() +
                                    " also mapped by glyph '" + existing->second + "'";
                return Result<Font>::failure(std::move(duplicate));
            }
            font.unicodeToGlyph[unicode] = normalized.name;
        }
        font.glyphOrder.push_back(normalized.name);
        font.glyphs.emplace(normalized.name, std::move(normalized));
    }
    font.fingerprint = fontFingerprint(font);
    return Result<Font>::success(std::move(font));
}

Result<Font> loadFontFile(const std::string& path, const FontLoadOptions& options) {
    auto document = Json::parseFile(path);
    if (!document.ok()) {
        Diagnostic diagnostic = std::move(document.error);
        if (diagnostic.code != ErrorCode::IoError) {
            diagnostic.code = ErrorCode::FontInvalid;
        }
        return Result<Font>::failure(std::move(diagnostic));
    }
    auto font = parseFont(*document.value, options);
    if (!font.ok() && font.error.code == ErrorCode::IoError) {
        font.error.code = ErrorCode::FontInvalid;
    }
    return font;
}

}  // namespace font_atlas
