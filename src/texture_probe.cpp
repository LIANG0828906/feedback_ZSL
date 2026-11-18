#include "json.h"
#include "output.h"
#include "texture.h"

#include <cstdlib>
#include <iostream>
#include <string>

int main(int argc, char** argv) {
    std::uint32_t seed = 0;
    std::string id;
    int resolution = 0;
    cr::Vec3 color{0.76, 0.43, 0.17};
    std::string tablePath;
    for (int i = 1; i < argc; ++i) {
        const std::string arg = argv[i];
        auto next = [&]() -> std::string {
            if (i + 1 >= argc) return {};
            return argv[++i];
        };
        if (arg == "--seed") {
            seed = static_cast<std::uint32_t>(std::strtoull(next().c_str(), nullptr, 10));
        } else if (arg == "--id") {
            id = next();
        } else if (arg == "--resolution") {
            resolution = std::atoi(next().c_str());
        } else if (arg == "--color") {
            color.x = std::atof(next().c_str());
            color.y = std::atof(next().c_str());
            color.z = std::atof(next().c_str());
        } else if (arg == "--table") {
            tablePath = next();
        } else {
            std::cerr << "unknown option: " << arg << "\n";
            return 1;
        }
    }
    if (id.empty() || resolution < 8) {
        std::cerr << "usage: coffered_texture_probe --seed N --id ID --resolution N "
                     "[--color r g b] [--table file.bin]\n";
        return 1;
    }
    const cr::WoodTexture texture =
        cr::generateWoodTexture(seed, id, resolution, color);
    if (!tablePath.empty()) {
        std::string error;
        if (!cr::writeFileAtomic(tablePath, cr::serializeTextureTable(texture), error)) {
            std::cerr << error << "\n";
            return 1;
        }
    }
    cr::json::Value report = cr::json::Value::object();
    report.set("seed", cr::json::Value(static_cast<double>(seed)));
    report.set("componentId", cr::json::Value(id));
    report.set("resolution", cr::json::Value(resolution));
    report.set("digest", cr::json::Value(texture.digest));
    report.set("tableBytes",
               cr::json::Value(static_cast<double>(
                   cr::serializeTextureTable(texture).size())));
    std::cout << report.canonical() << "\n";
    return 0;
}
