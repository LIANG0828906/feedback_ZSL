#include "core.hpp"

#include <cmath>
#include <cstddef>
#include <iostream>
#include <string>
#include <vector>

namespace {

int failures = 0;

void check(bool condition, const std::string& message) {
    if (!condition) {
        std::cerr << "FAIL: " << message << "\n";
        ++failures;
    }
}

std::string rectangle(double x0, double y0, double x1, double y1,
                      const std::string& direction,
                      const std::string& role = "") {
    std::vector<std::pair<double, double>> points;
    if (direction == "ccw") {
        points = {{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}};
    } else {
        points = {{x0, y0}, {x0, y1}, {x1, y1}, {x1, y0}};
    }
    points.push_back(points.front());
    std::string json = "{\"direction\":\"" + direction + "\"";
    if (!role.empty()) {
        json += ",\"role\":\"" + role + "\"";
    }
    json += ",\"start\":[" + std::to_string(points[0].first) + "," +
            std::to_string(points[0].second) + "],\"segments\":[";
    for (std::size_t i = 1; i < points.size(); ++i) {
        if (i > 1) {
            json += ",";
        }
        json += "{\"type\":\"line\",\"to\":[" +
                std::to_string(points[i].first) + "," +
                std::to_string(points[i].second) + "]}";
    }
    json += "]}";
    return json;
}

std::string nestedPath(bool sameDirection, const std::string& rule) {
    return "{\"fillRule\":\"" + rule +
           "\",\"budget\":{\"vertexMerge\":0.001,"
           "\"intersectionClassify\":0.001,\"sampleDistance\":0.01,"
           "\"totalLimit\":2},\"contours\":[" +
           rectangle(0, 0, 100, 80, "ccw") + "," +
           rectangle(20, 20, 80, 60, sameDirection ? "ccw" : "cw",
                     sameDirection ? "" : "hole") +
           "]}";
}

fill::ParseResult parse(const std::string& json,
                        const std::string& injection = "") {
    fill::ParseOptions options;
    options.injection = injection;
    return fill::parsePathJson(json, options, "test");
}

const fill::PixelDecision& center(const fill::RenderResult& render, int width,
                                  int height) {
    return render.pixels[static_cast<std::size_t>((height / 2) * width +
                                                  width / 2)];
}

void testNestedRules() {
    const int width = 80;
    const int height = 64;
    const fill::ParseResult hole = parse(nestedPath(false, "nonzero"));
    check(hole.path.has_value(), "nested hole path parses");
    if (hole.path.has_value()) {
        fill::RenderOptions options;
        options.width = width;
        options.height = height;
        options.degradeRatio = 0.5;
        const fill::RenderResult render =
            fill::renderPath(*hole.path, options);
        const fill::PixelDecision& pixel = center(render, width, height);
        check(pixel.nonzeroDefinite && !pixel.nonzeroFilled,
              "nonzero hole is empty");
        check(pixel.evenOddDefinite && !pixel.evenOddFilled,
              "even-odd hole is empty");
        check(!pixel.uncertain, "center of nested hole is not uncertain");
    }

    const fill::ParseResult same =
        parse(nestedPath(true, "nonzero"));
    check(same.path.has_value(), "same-direction nested path parses");
    if (same.path.has_value()) {
        fill::RenderOptions options;
        options.width = width;
        options.height = height;
        options.degradeRatio = 0.5;
        const fill::RenderResult render =
            fill::renderPath(*same.path, options);
        const fill::PixelDecision& pixel = center(render, width, height);
        check(pixel.nonzeroDefinite && pixel.nonzeroFilled,
              "nonzero same-direction interior is filled");
        check(pixel.evenOddDefinite && !pixel.evenOddFilled,
              "even-odd same-direction interior is empty");
        check(pixel.ruleSensitive,
              "same-direction interior is rule sensitive");
        check(pixel.windingLower == 2 && pixel.windingUpper == 2,
              "sensitive pixel exposes winding 2 bounds");
    }
}

void testInvalidInputs() {
    const fill::ParseResult missing = parse(
        "{\"fillRule\":\"nonzero\",\"contours\":[{\"direction\":\"ccw\","
        "\"start\":[0,0],\"segments\":[]}]}");
    check(!missing.path.has_value(), "empty contour list is rejected");
    check(missing.error.code == "E_PATH_INVALID", "missing segments code");
    check(missing.error.contour == 0 && missing.error.segment == -1,
          "missing segments has contour location");

    const fill::ParseResult unclosed = parse(
        "{\"fillRule\":\"nonzero\",\"contours\":[{\"direction\":\"ccw\","
        "\"start\":[0,0],\"segments\":[{\"type\":\"line\",\"to\":[10,0]},"
        "{\"type\":\"line\",\"to\":[10,10]},{\"type\":\"line\","
        "\"to\":[0,10]}]}]}");
    check(!unclosed.path.has_value(), "unclosed contour is rejected");
    check(unclosed.error.code == "E_PATH_INVALID", "unclosed code");
    check(unclosed.error.contour == 0 && unclosed.error.segment == 2,
          "unclosed error points at last segment");

    const fill::ParseResult nonfinite = parse(
        "{\"fillRule\":\"nonzero\",\"contours\":[{\"direction\":\"ccw\","
        "\"start\":[0,0],\"segments\":[{\"type\":\"line\","
        "\"to\":[NaN,0]},{\"type\":\"line\",\"to\":[10,10]},"
        "{\"type\":\"line\",\"to\":[0,10]},{\"type\":\"line\","
        "\"to\":[0,0]}]}]}");
    check(!nonfinite.path.has_value(), "non-finite coordinate is rejected");
    check(nonfinite.error.code == "E_PATH_INVALID",
          "non-finite coordinate code");
    check(nonfinite.error.contour == 0 && nonfinite.error.segment == 0,
          "non-finite coordinate location");
}

void testInjectionCodes() {
    const std::string valid = nestedPath(false, "nonzero");
    struct Case {
        const char* injection;
        const char* code;
    };
    const Case cases[] = {
        {"overlap-edge", "E_INJECT_OVERLAP_EDGE"},
        {"degenerate-segment", "E_INJECT_DEGENERATE_SEGMENT"},
        {"zero-area", "E_INJECT_ZERO_AREA_CONTOUR"},
        {"hole-direction", "E_INJECT_HOLE_DIRECTION"},
        {"nonfinite", "E_INJECT_NONFINITE_COORD"},
    };
    for (const Case& item : cases) {
        const fill::ParseResult result = parse(valid, item.injection);
        check(!result.path.has_value(), std::string("injection rejected: ") +
                                            item.injection);
        check(result.error.code == item.code,
              std::string("injection code: ") + item.code);
        check(result.error.injected, "injection flag is set");
        check(result.error.contour >= 0 && result.error.segment >= 0,
              "injection has source location");
        check(!result.error.relatedContours.empty(),
              "injection has related contour location");
    }
}

void testBudgetValidation() {
    const std::string zero =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":0,"
        "\"intersectionClassify\":0.001,\"sampleDistance\":0.25,"
        "\"totalLimit\":2},\"contours\":[" +
        rectangle(0, 0, 10, 10, "ccw") + "]}";
    const fill::ParseResult result = parse(zero);
    check(!result.path.has_value(), "zero tolerance rejected");
    check(result.error.code == "E_TOLERANCE_INVALID",
          "zero tolerance code");

    const std::string over =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":1,"
        "\"intersectionClassify\":1,\"sampleDistance\":1,"
        "\"totalLimit\":2},\"contours\":[" +
        rectangle(0, 0, 10, 10, "ccw") + "]}";
    const fill::ParseResult overResult = parse(over);
    check(!overResult.path.has_value(), "budget over limit rejected");
    check(overResult.error.code == "E_TOLERANCE_INVALID",
          "budget over limit code");

    const std::string negative =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":-0.1,"
        "\"intersectionClassify\":0.001,\"sampleDistance\":0.01,"
        "\"totalLimit\":2},\"contours\":[" +
        rectangle(0, 0, 10, 10, "ccw") + "]}";
    const fill::ParseResult negativeResult = parse(negative);
    check(!negativeResult.path.has_value(), "negative tolerance rejected");
    check(negativeResult.error.code == "E_TOLERANCE_INVALID",
          "negative tolerance code");
}

void testBezierAndDeterminism() {
    const std::string bezier =
        R"({"fillRule":"nonzero","budget":{"vertexMerge":0.001,"intersectionClassify":0.001,"sampleDistance":0.01,"totalLimit":2},"contours":[{"direction":"ccw","start":[0,0],"segments":[{"type":"line","to":[40,0]},{"type":"quadratic","control1":[60,20],"to":[40,40]},{"type":"cubic","control1":[20,60],"control2":[0,40],"to":[0,0]}]}]})";
    const fill::ParseResult parsed = parse(bezier);
    check(parsed.path.has_value(), "quadratic and cubic path parses");
    if (!parsed.path.has_value()) {
        return;
    }
    fill::RenderOptions options;
    options.width = 40;
    options.height = 40;
    options.degradeRatio = 1.0;
    const fill::RenderResult first = fill::renderPath(*parsed.path, options);
    const fill::RenderResult second = fill::renderPath(*parsed.path, options);
    check(first.error.ok(), "bezier render succeeds");
    check(first.coverage[0] == second.coverage[0] &&
              first.coverage[1] == second.coverage[1] &&
              first.fillMap == second.fillMap,
          "coverage and fill maps are byte deterministic");
    check(first.diagnostics.intersections.size() >=
              second.diagnostics.intersections.size(),
          "intersection diagnostics are present");
}

void testCancellation() {
    const fill::ParseResult parsed = parse(nestedPath(false, "nonzero"));
    check(parsed.path.has_value(), "cancel sample parses");
    if (!parsed.path.has_value()) {
        return;
    }
    fill::RenderOptions options;
    options.width = 64;
    options.height = 64;
    options.blockSize = 8;
    int checks = 0;
    const fill::RenderResult result =
        fill::renderPath(*parsed.path, options, [&checks]() {
            ++checks;
            return checks > 2;
        });
    check(result.cancelled, "render cancels at block checkpoint");
    check(result.noticeCode == "E_CANCELLED", "cancel notice code");
    check(result.fillMap.empty(), "cancelled render publishes no bitmap bytes");
}

void testClosureAndConflict() {
    const std::string implicit =
        "{\"fillRule\":\"nonzero\",\"contours\":[{\"direction\":\"ccw\","
        "\"closed\":true,\"start\":[0,0],\"segments\":["
        "{\"type\":\"line\",\"to\":[10,0]},"
        "{\"type\":\"line\",\"to\":[10,10]},"
        "{\"type\":\"line\",\"to\":[0,10]}]}]}";
    const fill::ParseResult closed = parse(implicit);
    check(closed.path.has_value(), "closed marker inserts closure segment");
    if (closed.path.has_value()) {
        check(closed.path->contours[0].segments.size() == 4,
              "implicit closure adds one segment");
    }

    const std::string wrongDirection =
        "{\"fillRule\":\"nonzero\",\"contours\":[{\"direction\":\"cw\","
        "\"start\":[0,0],\"segments\":[{\"type\":\"line\",\"to\":[10,0]},"
        "{\"type\":\"line\",\"to\":[10,10]},"
        "{\"type\":\"line\",\"to\":[0,10]},"
        "{\"type\":\"line\",\"to\":[0,0]}]}]}";
    const fill::ParseResult conflict = parse(wrongDirection);
    check(!conflict.path.has_value(), "direction mismatch is rejected");
    check(conflict.error.code == "E_FILL_RULE_CONFLICT",
          "direction mismatch conflict code");
    check(!conflict.error.relatedContours.empty(),
          "conflict lists related contour");

    const std::string sameDirectionHole =
        "{\"fillRule\":\"nonzero\",\"contours\":[" +
        rectangle(0, 0, 100, 80, "ccw") + "," +
        rectangle(20, 20, 80, 60, "ccw", "hole") + "]}";
    const fill::ParseResult roleConflict = parse(sameDirectionHole);
    check(!roleConflict.path.has_value(),
          "same-direction nonzero hole role is rejected");
    check(roleConflict.error.code == "E_FILL_RULE_CONFLICT",
          "hole role conflict code");
    check(roleConflict.error.relatedContours.size() == 2,
          "hole role conflict lists both contours");
}

void testDiagnosticsAndBudget() {
    const std::string near =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":0.001,"
        "\"intersectionClassify\":0.001,\"sampleDistance\":0.01,"
        "\"totalLimit\":2},\"contours\":[" +
        rectangle(0, 0, 100, 80, "ccw") + "," +
        rectangle(0, 0.0002, 100, 80.0002, "ccw") + "]}";
    const fill::ParseResult parsed = parse(near);
    check(parsed.path.has_value(), "near-coincident path parses");
    if (!parsed.path.has_value()) {
        return;
    }
    const fill::FlatPath flat = fill::flattenPath(
        *parsed.path, parsed.path->budget.intersectionClassify);
    const fill::Diagnostics diagnostics = fill::diagnosePath(
        *parsed.path, flat, parsed.path->budget.intersectionClassify);
    check(diagnostics.monotonicSegmentCount > 0,
          "diagnostics expose monotonic segment count");
    check(!diagnostics.nearCoincident.empty(),
          "near-coincident segment pairs are reported");
    check(diagnostics.nearCoincident.front().confidence == "high" ||
              diagnostics.nearCoincident.front().confidence == "medium" ||
              diagnostics.nearCoincident.front().confidence == "low",
          "near-coincident pair has confidence");

    const std::string degenerate =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":0.001,"
        "\"intersectionClassify\":0.001,\"sampleDistance\":0.01,"
        "\"totalLimit\":2},\"contours\":[{\"direction\":\"ccw\","
        "\"start\":[0,0],\"segments\":["
        "{\"type\":\"line\",\"to\":[0.0001,0]},"
        "{\"type\":\"line\",\"to\":[10,0]},"
        "{\"type\":\"line\",\"to\":[10,10]},"
        "{\"type\":\"line\",\"to\":[0,10]},"
        "{\"type\":\"line\",\"to\":[0,0]}]}]}";
    const fill::ParseResult degenerateParsed = parse(degenerate);
    check(degenerateParsed.path.has_value(), "degenerate sample parses");
    if (degenerateParsed.path.has_value()) {
        const fill::FlatPath degenerateFlat = fill::flattenPath(
            *degenerateParsed.path,
            degenerateParsed.path->budget.intersectionClassify);
        const fill::Diagnostics degenerateDiagnostics = fill::diagnosePath(
            *degenerateParsed.path, degenerateFlat,
            degenerateParsed.path->budget.intersectionClassify);
        check(!degenerateDiagnostics.degenerate.empty(),
              "short segment diagnostic is reported");
    }

    fill::RenderOptions options;
    options.width = 24;
    options.height = 24;
    options.degradeRatio = 1.0;
    fill::ToleranceBudget previous = parsed.path->budget;
    previous.sampleDistance *= 2.0;
    const fill::BudgetAnalysis analysis =
        fill::analyzeBudget(*parsed.path, options, previous);
    check(analysis.tighteningOrder.size() == 3,
          "three fixed tightening steps are declared");
    check(analysis.tighteningOrder[0] == "intersection_classify" &&
              analysis.tighteningOrder[1] == "sample_distance" &&
              analysis.tighteningOrder[2] == "vertex_merge",
          "tightening order is stable");
    check(analysis.impacts.size() == 3,
          "each tolerance has an impact record");
}

void testPixelStateIsolation() {
    const std::string path =
        "{\"fillRule\":\"nonzero\",\"budget\":{\"vertexMerge\":0.001,"
        "\"intersectionClassify\":0.001,\"sampleDistance\":3,"
        "\"totalLimit\":10},\"contours\":[" +
        rectangle(0, 0, 100, 80, "ccw") + "," +
        rectangle(20, 20, 80, 60, "ccw") + "]}";
    const fill::ParseResult parsed = parse(path);
    check(parsed.path.has_value(), "pixel isolation sample parses");
    if (!parsed.path.has_value()) {
        return;
    }
    fill::RenderOptions options;
    options.width = 32;
    options.height = 32;
    options.degradeRatio = 1.0;
    const fill::RenderResult render = fill::renderPath(*parsed.path, options);
    bool sawUncertain = false;
    bool sawSensitive = false;
    for (std::size_t i = 0; i < render.pixels.size(); ++i) {
        const fill::PixelDecision& pixel = render.pixels[i];
        if (pixel.uncertain) {
            sawUncertain = true;
            check(render.coverage[0][i] == 128 &&
                      render.coverage[1][i] == 128 &&
                      render.fillMap[i] == 128 &&
                      render.sourceMap[i] == 170,
                  "uncertain pixel is never written as solid or empty");
            check(pixel.windingLower <= pixel.windingUpper,
                  "uncertain winding bounds are ordered");
        }
        if (pixel.ruleSensitive) {
            sawSensitive = true;
            check(render.diffMap[i] == 255 && render.sourceMap[i] == 85,
                  "rule-sensitive pixel source is recorded");
            check(pixel.nearestBoundaryWorld >= 0.0,
                  "sensitive pixel exposes boundary distance");
        }
    }
    check(sawUncertain, "relaxed sample contains uncertain pixels");
    check(sawSensitive, "same-direction sample contains sensitive pixels");
}

}  // namespace

int main() {
    testNestedRules();
    testInvalidInputs();
    testInjectionCodes();
    testBudgetValidation();
    testBezierAndDeterminism();
    testCancellation();
    testClosureAndConflict();
    testDiagnosticsAndBudget();
    testPixelStateIsolation();
    if (failures != 0) {
        std::cerr << failures << " test(s) failed\n";
        return 1;
    }
    std::cout << "all tests passed\n";
    return 0;
}
