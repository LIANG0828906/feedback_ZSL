#include "core.hpp"

#include <cstdlib>
#include <iostream>
#include <sstream>
#include <string>
#include <vector>

namespace {

void printUsage() {
    std::cout
        << "fill_pipeline 1.0\n"
        << "Usage:\n"
        << "  fill_pipeline render --input PATH --out DIR [options]\n"
        << "  fill_pipeline submit --result DIR/result.json\n"
        << "  fill_pipeline generate-samples --out DIR [--width N --height N]\n"
        << "\n"
        << "Render options:\n"
        << "  --width N --height N\n"
        << "  --degrade-ratio R\n"
        << "  --budget V,I,S,T\n"
        << "  --previous-budget V,I,S,T --compare-previous\n"
        << "  --inject overlap-edge|degenerate-segment|zero-area|"
           "hole-direction|nonfinite\n"
        << "  --cancel-after-blocks N\n";
}

struct Args {
    std::string command;
    std::vector<std::pair<std::string, std::string>> values;

    bool has(const std::string& key) const {
        for (const auto& item : values) {
            if (item.first == key) {
                return true;
            }
        }
        return false;
    }

    std::string get(const std::string& key,
                    const std::string& fallback = "") const {
        for (const auto& item : values) {
            if (item.first == key) {
                return item.second;
            }
        }
        return fallback;
    }
};

bool parseInteger(const std::string& text, int& value) {
    try {
        std::size_t used = 0;
        const long parsed = std::stol(text, &used, 10);
        if (used != text.size() || parsed < -2147483647L || parsed > 2147483647L) {
            return false;
        }
        value = static_cast<int>(parsed);
        return true;
    } catch (...) {
        return false;
    }
}

bool parseReal(const std::string& text, double& value) {
    try {
        std::size_t used = 0;
        value = std::stod(text, &used);
        return used == text.size();
    } catch (...) {
        return false;
    }
}

bool parseBudget(const std::string& text, fill::ToleranceBudget& budget) {
    std::vector<std::string> fields;
    std::stringstream stream(text);
    std::string field;
    while (std::getline(stream, field, ',')) {
        fields.push_back(field);
    }
    if (fields.size() != 3 && fields.size() != 4) {
        return false;
    }
    std::vector<double> values(fields.size());
    for (std::size_t i = 0; i < fields.size(); ++i) {
        if (!parseReal(fields[i], values[i])) {
            return false;
        }
    }
    budget.vertexMerge = values[0];
    budget.intersectionClassify = values[1];
    budget.sampleDistance = values[2];
    if (values.size() == 4) {
        budget.totalLimit = values[3];
    } else {
        budget.totalLimit =
            budget.vertexMerge + budget.intersectionClassify +
            budget.sampleDistance;
    }
    return true;
}

fill::Error argumentError(const std::string& detail) {
    fill::Error error;
    error.code = "E_ARGUMENT_INVALID";
    error.detail = detail;
    return error;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) {
        printUsage();
        return 1;
    }
    Args args;
    args.command = argv[1];
    if (args.command == "--help" || args.command == "-h" ||
        args.command == "help") {
        printUsage();
        return 0;
    }
    if (args.command == "--version" || args.command == "version") {
        std::cout << "fill_pipeline 1.0\n";
        return 0;
    }
    for (int i = 2; i < argc; ++i) {
        std::string token = argv[i];
        if (token.rfind("--", 0) != 0) {
            std::cout << fill::errorToJson(
                             argumentError("unexpected argument: " + token))
                      << "\n";
            return 1;
        }
        token = token.substr(2);
        const std::size_t equals = token.find('=');
        if (equals != std::string::npos) {
            args.values.emplace_back(token.substr(0, equals),
                                     token.substr(equals + 1));
            continue;
        }
        if (i + 1 >= argc || std::string(argv[i + 1]).rfind("--", 0) == 0) {
            args.values.emplace_back(token, "true");
            continue;
        }
        args.values.emplace_back(token, argv[++i]);
    }

    if (args.command == "submit") {
        const std::string result = args.get("result");
        if (result.empty()) {
            std::cout << fill::errorToJson(
                             argumentError("--result is required"))
                      << "\n";
            return 1;
        }
        return fill::runSubmit(result);
    }
    if (args.command == "generate-samples") {
        int width = 64;
        int height = 64;
        if (args.has("width") && !parseInteger(args.get("width"), width)) {
            std::cout << fill::errorToJson(
                             argumentError("invalid --width"))
                      << "\n";
            return 1;
        }
        if (args.has("height") && !parseInteger(args.get("height"), height)) {
            std::cout << fill::errorToJson(
                             argumentError("invalid --height"))
                      << "\n";
            return 1;
        }
        return fill::runGenerateSamples(args.get("out"), width, height);
    }
    if (args.command != "render") {
        std::cout << fill::errorToJson(
                         argumentError("unknown command: " + args.command))
                  << "\n";
        printUsage();
        return 1;
    }

    fill::PipelineOptions options;
    options.inputFile = args.get("input");
    options.outputDir = args.get("out");
    options.injection = args.get("inject");
    if (args.has("width") && !parseInteger(args.get("width"), options.width)) {
        std::cout << fill::errorToJson(argumentError("invalid --width"))
                  << "\n";
        return 1;
    }
    if (args.has("height") &&
        !parseInteger(args.get("height"), options.height)) {
        std::cout << fill::errorToJson(argumentError("invalid --height"))
                  << "\n";
        return 1;
    }
    if (args.has("degrade-ratio") &&
        !parseReal(args.get("degrade-ratio"), options.degradeRatio)) {
        std::cout << fill::errorToJson(
                         argumentError("invalid --degrade-ratio"))
                  << "\n";
        return 1;
    }
    if (args.has("budget") &&
        !parseBudget(args.get("budget"), options.budget)) {
        std::cout << fill::errorToJson(argumentError("invalid --budget"))
                  << "\n";
        return 1;
    }
    options.budgetProvided = args.has("budget");
    if (args.has("previous-budget")) {
        if (!parseBudget(args.get("previous-budget"), options.previousBudget)) {
            std::cout << fill::errorToJson(
                             argumentError("invalid --previous-budget"))
                      << "\n";
            return 1;
        }
        options.comparePrevious = true;
    }
    if (args.has("cancel-after-blocks") &&
        !parseInteger(args.get("cancel-after-blocks"),
                      options.cancelAfterBlocks)) {
        std::cout << fill::errorToJson(
                         argumentError("invalid --cancel-after-blocks"))
                  << "\n";
        return 1;
    }
    return fill::runRenderPipeline(options);
}
