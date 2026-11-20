#include <filesystem>
#include <iostream>
#include <string>

#include "shadowpipe/synthetic.hpp"

namespace {

void printUsage() {
    std::cout
        << "Usage: scene_gen [--out-dir <directory>] [--variant valid|bad-index|"
           "bad-transform|bad-bounds]\n"
        << "Without --variant, all deterministic samples are written.\n";
}

}  // namespace

int main(int argc, char** argv) {
    std::string output_directory = "data";
    std::string variant_name;
    for (int i = 1; i < argc; ++i) {
        const std::string argument = argv[i];
        if (argument == "--help" || argument == "-h") {
            printUsage();
            return 0;
        }
        if (argument == "--out-dir" && i + 1 < argc) {
            output_directory = argv[++i];
            continue;
        }
        if (argument == "--variant" && i + 1 < argc) {
            variant_name = argv[++i];
            continue;
        }
        std::cerr << "unknown or incomplete argument: " << argument << "\n";
        return 2;
    }

    shadowpipe::Status status;
    if (variant_name.empty()) {
        status = shadowpipe::generateAllSyntheticSamples(output_directory);
        if (status.ok()) {
            std::cout << "generated deterministic samples under " << output_directory
                      << "\n";
        }
    } else {
        shadowpipe::SyntheticVariant variant = shadowpipe::SyntheticVariant::Valid;
        std::string filename = "synthetic.rscene";
        if (variant_name == "valid") {
            variant = shadowpipe::SyntheticVariant::Valid;
        } else if (variant_name == "bad-index") {
            variant = shadowpipe::SyntheticVariant::BadIndex;
            filename = "invalid_index.rscene";
        } else if (variant_name == "bad-transform") {
            variant = shadowpipe::SyntheticVariant::BadTransform;
            filename = "invalid_transform.rscene";
        } else if (variant_name == "bad-bounds") {
            variant = shadowpipe::SyntheticVariant::BadBounds;
            filename = "invalid_bounds.rscene";
        } else {
            std::cerr << "unknown variant: " << variant_name << "\n";
            return 2;
        }
        std::error_code ec;
        std::filesystem::create_directories(output_directory, ec);
        if (ec) {
            std::cerr << "cannot create output directory\n";
            return 7;
        }
        const std::string path =
            (std::filesystem::path(output_directory) / filename).string();
        status = shadowpipe::writeSyntheticScene(path, variant);
        if (status.ok()) {
            std::cout << "wrote " << path << "\n";
        }
    }
    if (!status.ok()) {
        std::cerr << shadowpipe::errorCodeName(status.code) << ": "
                  << status.message << "\n";
        return 7;
    }
    return 0;
}

