#include "core.hpp"
#include "json.hpp"

#include <algorithm>
#include <cctype>
#include <cmath>
#include <fstream>
#include <limits>
#include <set>
#include <sstream>

namespace fill {
namespace {

Error pathError(const std::string& detail, int contour = -1, int segment = -1) {
    Error error;
    error.code = "E_PATH_INVALID";
    error.contour = contour;
    error.segment = segment;
    error.detail = detail;
    return error;
}

std::string lower(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(),
                   [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return value;
}

std::string normalizedWord(std::string value) {
    value = lower(std::move(value));
    value.erase(std::remove_if(value.begin(), value.end(), [](char c) {
                    return c == '-' || c == '_' || c == ' ';
                }),
                value.end());
    return value;
}

const json::Value* findAny(const json::Value& object,
                           std::initializer_list<const char*> keys) {
    if (!object.isObject()) {
        return nullptr;
    }
    for (const char* key : keys) {
        const json::Value* value = object.find(key);
        if (value != nullptr) {
            return value;
        }
    }
    return nullptr;
}

bool parsePoint(const json::Value& value, Vec2& point) {
    if (value.isArray()) {
        if (value.array.size() != 2 || !value.array[0].isNumber() ||
            !value.array[1].isNumber()) {
            return false;
        }
        point = {value.array[0].number, value.array[1].number};
        return true;
    }
    if (value.isObject()) {
        const json::Value* x = value.find("x");
        const json::Value* y = value.find("y");
        if (x == nullptr || y == nullptr || !x->isNumber() || !y->isNumber()) {
            return false;
        }
        point = {x->number, y->number};
        return true;
    }
    return false;
}

std::optional<Direction> parseDirection(const std::string& text) {
    const std::string word = normalizedWord(text);
    if (word == "ccw" || word == "counterclockwise" || word == "anticlockwise" ||
        word == "1" || word == "positive") {
        return Direction::CounterClockwise;
    }
    if (word == "cw" || word == "clockwise" || word == "-1" ||
        word == "negative") {
        return Direction::Clockwise;
    }
    return std::nullopt;
}

std::optional<FillRule> parseFillRule(const std::string& text) {
    const std::string word = normalizedWord(text);
    if (word == "nonzero" || word == "winding") {
        return FillRule::NonZero;
    }
    if (word == "evenodd" || word == "parity") {
        return FillRule::EvenOdd;
    }
    return std::nullopt;
}

}  // namespace

bool validateToleranceBudget(const ToleranceBudget& budget, std::string& detail) {
    const double values[] = {budget.vertexMerge, budget.intersectionClassify,
                             budget.sampleDistance, budget.totalLimit};
    for (double value : values) {
        if (!std::isfinite(value) || value <= 0.0) {
            detail = "tolerance values must be finite and positive";
            return false;
        }
    }
    const double sum = budget.vertexMerge + budget.intersectionClassify +
                       budget.sampleDistance;
    if (sum > budget.totalLimit) {
        detail = "tolerance budget exceeds totalLimit";
        return false;
    }
    return true;
}

namespace {

bool readNumberAlias(const json::Value& object,
                     std::initializer_list<const char*> keys, double& output) {
    const json::Value* value = findAny(object, keys);
    if (value == nullptr) {
        return false;
    }
    if (!value->isNumber()) {
        output = std::numeric_limits<double>::quiet_NaN();
        return true;
    }
    output = value->number;
    return true;
}

std::pair<int, int> firstLocation(const json::Value& root) {
    const json::Value* contours =
        findAny(root, {"contours", "subpaths", "subContours"});
    if (contours == nullptr || !contours->isArray() || contours->array.empty()) {
        return {0, 0};
    }
    const json::Value* segments =
        findAny(contours->array[0], {"segments", "path", "edges"});
    if (segments == nullptr || !segments->isArray() || segments->array.empty()) {
        return {0, 0};
    }
    return {0, 0};
}

Error applyInjection(const std::string& injection, const json::Value& root) {
    if (injection.empty()) {
        return {};
    }
    const std::string kind = normalizedWord(injection);
    const auto location = firstLocation(root);
    Error error;
    error.injected = true;
    error.injectionClass = kind;
    error.contour = location.first;
    error.segment = location.second;
    int contourCount = 0;
    const json::Value* contours =
        findAny(root, {"contours", "subpaths", "subContours"});
    if (contours != nullptr && contours->isArray()) {
        contourCount = static_cast<int>(contours->array.size());
    }
    error.relatedContours.push_back(0);
    if (contourCount > 1) {
        error.relatedContours.push_back(1);
    }

    if (kind == "overlapedge" || kind == "overlap" ||
        kind == "coincidentedgepair") {
        error.code = "E_INJECT_OVERLAP_EDGE";
        error.detail = "injected coincident edge contour pair at parse entry";
    } else if (kind == "degeneratesegment" || kind == "degenerate" ||
               kind == "shortsegment") {
        error.code = "E_INJECT_DEGENERATE_SEGMENT";
        error.detail = "injected extremely short segment at parse entry";
    } else if (kind == "zeroarea" || kind == "zeroareacontour" ||
               kind == "zeroareacontours") {
        error.code = "E_INJECT_ZERO_AREA_CONTOUR";
        error.detail = "injected zero-area contour at parse entry";
    } else if (kind == "holedirection" || kind == "holereversed" ||
               kind == "holedirectionreversal") {
        error.code = "E_INJECT_HOLE_DIRECTION";
        error.detail = "injected nested hole direction reversal at parse entry";
    } else if (kind == "nonfinite" || kind == "nonfinitecoordinate" ||
               kind == "nan" || kind == "infinity") {
        error.code = "E_INJECT_NONFINITE_COORD";
        error.detail = "injected non-finite coordinate at parse entry";
    } else {
        error.code = "E_INJECTION_UNKNOWN";
        error.detail = "unknown injection class: " + injection;
    }
    return error;
}

std::optional<SegmentType> parseSegmentType(const std::string& text) {
    const std::string word = normalizedWord(text);
    if (word == "line" || word == "l" || word == "lineto") {
        return SegmentType::Line;
    }
    if (word == "quad" || word == "quadratic" || word == "q" ||
        word == "quadraticbezier" || word == "quadraticbeziercurveto") {
        return SegmentType::Quadratic;
    }
    if (word == "cubic" || word == "c" || word == "cubicbezier" ||
        word == "cubicbeziercurveto" || word == "curve") {
        return SegmentType::Cubic;
    }
    return std::nullopt;
}

}  // namespace

ParseResult parsePathJson(const std::string& text, const ParseOptions& options,
                          const std::string& sourceName) {
    ParseResult result;
    const json::ParseResult parsed = json::parse(text);
    if (!parsed.error.ok()) {
        result.error = pathError("json_parse_error: " + parsed.error.message);
        return result;
    }
    const json::Value& root = parsed.value;
    if (!root.isObject()) {
        result.error = pathError("root must be an object");
        return result;
    }

    const Error injectionError = applyInjection(options.injection, root);
    if (!injectionError.ok()) {
        result.error = injectionError;
        return result;
    }

    ToleranceBudget budget = options.budget;
    const json::Value* budgetValue =
        findAny(root, {"budget", "toleranceBudget", "tolerance_budget"});
    if (budgetValue != nullptr && !options.overrideBudget) {
        if (!budgetValue->isObject()) {
            Error error;
            error.code = "E_TOLERANCE_INVALID";
            error.detail = "budget must be an object";
            result.error = error;
            return result;
        }
        readNumberAlias(*budgetValue, {"vertexMerge", "vertex_merge"},
                        budget.vertexMerge);
        readNumberAlias(*budgetValue,
                        {"intersectionClassify", "intersection_classify"},
                        budget.intersectionClassify);
        readNumberAlias(*budgetValue, {"sampleDistance", "sample_distance"},
                        budget.sampleDistance);
        readNumberAlias(*budgetValue, {"totalLimit", "total_limit", "limit"},
                        budget.totalLimit);
    }
    if (!options.overrideBudget) {
        readNumberAlias(root, {"vertexMergeTolerance"}, budget.vertexMerge);
        readNumberAlias(root, {"intersectionTolerance"},
                        budget.intersectionClassify);
        readNumberAlias(root, {"sampleDistanceTolerance"},
                        budget.sampleDistance);
        readNumberAlias(root, {"toleranceLimit"}, budget.totalLimit);
    }

    std::string budgetDetail;
    if (!validateToleranceBudget(budget, budgetDetail)) {
        Error error;
        error.code = "E_TOLERANCE_INVALID";
        error.detail = budgetDetail;
        result.error = error;
        return result;
    }

    const json::Value* fillRuleValue =
        findAny(root, {"fillRule", "fill_rule", "rule", "windingRule"});
    if (fillRuleValue == nullptr || !fillRuleValue->isString()) {
        result.error = pathError("missing fill rule declaration");
        return result;
    }
    const auto fillRule = parseFillRule(fillRuleValue->string);
    if (!fillRule.has_value()) {
        result.error = pathError("unsupported fill rule: " +
                                 fillRuleValue->string);
        return result;
    }

    const json::Value* contoursValue =
        findAny(root, {"contours", "subpaths", "subContours"});
    if (contoursValue == nullptr || !contoursValue->isArray() ||
        contoursValue->array.empty()) {
        result.error = pathError("missing non-empty contour list");
        return result;
    }

    Path path;
    path.fillRule = *fillRule;
    path.budget = budget;
    path.sourceName = sourceName;

    for (std::size_t contourIndex = 0;
         contourIndex < contoursValue->array.size(); ++contourIndex) {
        const json::Value& contourValue = contoursValue->array[contourIndex];
        const int ci = static_cast<int>(contourIndex);
        if (!contourValue.isObject()) {
            result.error = pathError("contour must be an object", ci, -1);
            return result;
        }
        const json::Value* directionValue =
            findAny(contourValue, {"direction", "orientation", "dir"});
        if (directionValue == nullptr ||
            (!directionValue->isString() && !directionValue->isNumber())) {
            result.error =
                pathError("missing contour direction marker", ci, -1);
            return result;
        }
        const auto declaredDirection =
            directionValue->isString()
                ? parseDirection(directionValue->string)
                : (directionValue->number == 1.0
                       ? std::optional<Direction>(Direction::CounterClockwise)
                       : (directionValue->number == -1.0
                              ? std::optional<Direction>(Direction::Clockwise)
                              : std::nullopt));
        if (!declaredDirection.has_value()) {
            result.error =
                pathError("invalid contour direction: " +
                              (directionValue->isString()
                                   ? directionValue->string
                                   : formatDouble(directionValue->number)),
                          ci, -1);
            return result;
        }

        const json::Value* segmentsValue =
            findAny(contourValue, {"segments", "path", "edges"});
        if (segmentsValue == nullptr || !segmentsValue->isArray() ||
            segmentsValue->array.empty()) {
            result.error =
                pathError("missing non-empty segment list", ci, -1);
            return result;
        }

        const json::Value* startValue =
            findAny(contourValue, {"start", "moveTo", "origin"});
        if (startValue == nullptr) {
            startValue = findAny(segmentsValue->array.front(), {"from", "start"});
        }
        Vec2 current;
        if (startValue == nullptr || !parsePoint(*startValue, current)) {
            result.error = pathError("missing or invalid contour start point",
                                     ci, 0);
            return result;
        }
        if (!finite(current)) {
            result.error =
                pathError("non-finite start coordinate", ci, 0);
            return result;
        }

        Contour contour;
        contour.sourceIndex = ci;
        contour.declaredDirection = *declaredDirection;
        const json::Value* roleValue =
            findAny(contourValue, {"role", "contourRole"});
        if (roleValue != nullptr && roleValue->isString()) {
            const std::string role = normalizedWord(roleValue->string);
            contour.hasRole = true;
            contour.roleHole = (role == "hole" || role == "inner");
        }

        for (std::size_t segmentIndex = 0;
             segmentIndex < segmentsValue->array.size(); ++segmentIndex) {
            const int si = static_cast<int>(segmentIndex);
            const json::Value& segmentValue =
                segmentsValue->array[segmentIndex];
            if (!segmentValue.isObject()) {
                result.error =
                    pathError("segment must be an object", ci, si);
                return result;
            }
            const json::Value* typeValue =
                findAny(segmentValue, {"type", "kind", "command"});
            if (typeValue == nullptr || !typeValue->isString()) {
                result.error =
                    pathError("missing segment type", ci, si);
                return result;
            }
            const auto type = parseSegmentType(typeValue->string);
            if (!type.has_value()) {
                result.error =
                    pathError("unsupported segment type: " +
                                  typeValue->string,
                              ci, si);
                return result;
            }
            const json::Value* fromValue =
                findAny(segmentValue, {"from", "start"});
            if (fromValue != nullptr) {
                Vec2 specified;
                if (!parsePoint(*fromValue, specified)) {
                    result.error =
                        pathError("invalid segment start point", ci, si);
                    return result;
                }
                if (!finite(specified)) {
                    result.error =
                        pathError("non-finite segment start coordinate", ci, si);
                    return result;
                }
                if (distance(specified, current) > budget.vertexMerge) {
                    result.error =
                        pathError("segment start does not match previous end",
                                  ci, si);
                    return result;
                }
            }

            const json::Value* toValue =
                findAny(segmentValue, {"to", "end", "point", "p"});
            if (toValue == nullptr) {
                result.error =
                    pathError("missing segment endpoint", ci, si);
                return result;
            }
            Vec2 endpoint;
            if (!parsePoint(*toValue, endpoint)) {
                result.error =
                    pathError("invalid segment endpoint", ci, si);
                return result;
            }
            if (!finite(endpoint)) {
                result.error =
                    pathError("non-finite segment coordinate", ci, si);
                return result;
            }
            if (distance(endpoint, current) <= budget.vertexMerge) {
                endpoint = current;
            }
            double nearestVertexDistance =
                distance(endpoint, current);
            Vec2 nearestVertex = current;
            for (const Segment& prior : contour.segments) {
                const double p0Distance =
                    distance(endpoint, prior.p0);
                if (p0Distance <= budget.vertexMerge &&
                    p0Distance < nearestVertexDistance) {
                    nearestVertexDistance = p0Distance;
                    nearestVertex = prior.p0;
                }
                const double p1Distance =
                    distance(endpoint, prior.p1);
                if (p1Distance <= budget.vertexMerge &&
                    p1Distance < nearestVertexDistance) {
                    nearestVertexDistance = p1Distance;
                    nearestVertex = prior.p1;
                }
            }
            if (nearestVertexDistance <= budget.vertexMerge) {
                endpoint = nearestVertex;
            }

            Segment segment;
            segment.type = *type;
            segment.p0 = current;
            segment.p1 = endpoint;
            segment.sourceIndex = si;
            if (*type == SegmentType::Quadratic) {
                const json::Value* control =
                    findAny(segmentValue,
                            {"control1", "control", "c1", "controlPoint1"});
                if (control == nullptr || !parsePoint(*control, segment.c1)) {
                    result.error =
                        pathError("missing or invalid quadratic control point",
                                  ci, si);
                    return result;
                }
                if (!finite(segment.c1)) {
                    result.error =
                        pathError("non-finite quadratic control coordinate", ci,
                                  si);
                    return result;
                }
            } else if (*type == SegmentType::Cubic) {
                const json::Value* control1 =
                    findAny(segmentValue,
                            {"control1", "c1", "controlPoint1"});
                const json::Value* control2 =
                    findAny(segmentValue,
                            {"control2", "c2", "controlPoint2"});
                if (control1 == nullptr || control2 == nullptr ||
                    !parsePoint(*control1, segment.c1) ||
                    !parsePoint(*control2, segment.c2)) {
                    result.error =
                        pathError("missing or invalid cubic control points", ci,
                                  si);
                    return result;
                }
                if (!finite(segment.c1) || !finite(segment.c2)) {
                    result.error =
                        pathError("non-finite cubic control coordinate", ci, si);
                    return result;
                }
            }
            contour.segments.push_back(segment);
            contour.bounds.include(segment.p0);
            contour.bounds.include(segment.p1);
            if (*type != SegmentType::Line) {
                contour.bounds.include(segment.c1);
                if (*type == SegmentType::Cubic) {
                    contour.bounds.include(segment.c2);
                }
            }
            current = endpoint;
        }

        const json::Value* closedValue =
            findAny(contourValue, {"closed", "isClosed"});
        const bool declaresClosed =
            closedValue != nullptr && closedValue->isBool() &&
            closedValue->boolean;
        const bool declaresOpen =
            closedValue != nullptr && closedValue->isBool() &&
            !closedValue->boolean;
        if (declaresOpen) {
            result.error =
                pathError("subcontour must be closed", ci,
                          static_cast<int>(contour.segments.size()) - 1);
            return result;
        }
        double closureDistance = distance(current, contour.segments.front().p0);
        if (closureDistance > budget.vertexMerge && declaresClosed) {
            Segment closing;
            closing.type = SegmentType::Line;
            closing.p0 = current;
            closing.p1 = contour.segments.front().p0;
            closing.sourceIndex = static_cast<int>(contour.segments.size());
            contour.bounds.include(closing.p0);
            contour.bounds.include(closing.p1);
            contour.segments.push_back(closing);
            current = closing.p1;
            closureDistance = 0.0;
        }
        if (closureDistance > budget.vertexMerge) {
            result.error =
                pathError("contour is not closed", ci,
                          static_cast<int>(contour.segments.size()) - 1);
            return result;
        }
        contour.segments.back().p1 = contour.segments.front().p0;
        contour.vertices.reserve(contour.segments.size());
        for (const Segment& segment : contour.segments) {
            contour.vertices.push_back(segment.p0);
            path.bounds.include(segment.p0);
            path.bounds.include(segment.p1);
        }

        contour.signedArea = contourSignedAreaWorld(contour);
        const double areaTolerance =
            std::max(1e-12, budget.vertexMerge *
                                std::max(1.0, std::hypot(
                                                  contour.bounds.width(),
                                                  contour.bounds.height())));
        if (std::abs(contour.signedArea) <= areaTolerance) {
            Error error;
            error.code = "E_ZERO_AREA_CONTOUR";
            error.contour = ci;
            error.segment = -1;
            error.detail = "contour geometry has zero area";
            result.error = error;
            return result;
        }
        contour.actualDirection = contour.signedArea > 0.0
                                      ? Direction::CounterClockwise
                                      : Direction::Clockwise;
        if (contour.actualDirection != contour.declaredDirection) {
            Error error;
            error.code = "E_FILL_RULE_CONFLICT";
            error.contour = ci;
            error.segment = -1;
            error.detail =
                std::string("declared direction does not match geometry (") +
                (contour.actualDirection == Direction::CounterClockwise
                     ? "ccw"
                     : "cw") +
                ")";
            error.relatedContours.push_back(ci);
            result.error = error;
            return result;
        }
        if (contour.bounds.valid) {
            path.bounds.include({contour.bounds.minX, contour.bounds.minY});
            path.bounds.include({contour.bounds.maxX, contour.bounds.maxY});
        }
        path.contours.push_back(std::move(contour));
    }

    if (path.fillRule == FillRule::NonZero) {
        const FlatPath flat = flattenPath(path, std::max(1e-9, budget.vertexMerge));
        for (std::size_t i = 0; i < path.contours.size(); ++i) {
            const Contour& contour = path.contours[i];
            if (!contour.hasRole || !contour.roleHole) {
                continue;
            }
            int parent = -1;
            double parentArea = std::numeric_limits<double>::infinity();
            const Vec2 sample = flat.contourVertices[i].empty()
                                    ? Vec2{}
                                    : flat.contourVertices[i].front();
            for (std::size_t j = 0; j < path.contours.size(); ++j) {
                if (i == j) {
                    continue;
                }
                if (pointInPolygon(sample, flat.contourVertices[j])) {
                    const double area = std::abs(path.contours[j].signedArea);
                    if (area < parentArea) {
                        parentArea = area;
                        parent = static_cast<int>(j);
                    }
                }
            }
            if (parent < 0 ||
                path.contours[parent].actualDirection ==
                    contour.actualDirection) {
                Error error;
                error.code = "E_FILL_RULE_CONFLICT";
                error.contour = static_cast<int>(i);
                error.segment = -1;
                error.detail = parent < 0
                                   ? "hole role has no enclosing parent"
                                   : "nonzero hole direction matches parent";
                error.relatedContours.push_back(static_cast<int>(i));
                if (parent >= 0) {
                    error.relatedContours.push_back(parent);
                }
                result.error = error;
                return result;
            }
        }
    }

    result.path = std::move(path);
    return result;
}

ParseResult parsePathFile(const std::string& filename,
                          const ParseOptions& options) {
    std::ifstream input(filename, std::ios::binary);
    if (!input) {
        ParseResult result;
        result.error = pathError("cannot open input file: " + filename);
        return result;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    return parsePathJson(buffer.str(), options, filename);
}

}  // namespace fill
