#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <set>
#include <sstream>
#include <string>
#include <thread>
#include <vector>

#ifdef _WIN32
#include <conio.h>
#include <windows.h>
#else
#include <poll.h>
#include <termios.h>
#include <unistd.h>
#endif

#include "terrain/async_controller.h"
#include "terrain/sample_generator.h"

namespace {

class TerminalMode {
public:
    TerminalMode() {
#ifdef _WIN32
        HANDLE output = GetStdHandle(STD_OUTPUT_HANDLE);
        DWORD mode = 0;
        if (output != INVALID_HANDLE_VALUE &&
            GetConsoleMode(output, &mode)) {
            SetConsoleMode(
                output, mode | ENABLE_VIRTUAL_TERMINAL_PROCESSING);
        }
#else
        if (isatty(STDIN_FILENO)) {
            tcgetattr(STDIN_FILENO, &original_);
            termios raw = original_;
            raw.c_lflag &= static_cast<tcflag_t>(~(ICANON | ECHO));
            raw.c_cc[VMIN] = 0;
            raw.c_cc[VTIME] = 0;
            tcsetattr(STDIN_FILENO, TCSANOW, &raw);
            active_ = true;
        }
#endif
    }

    ~TerminalMode() {
#ifndef _WIN32
        if (active_) {
            tcsetattr(STDIN_FILENO, TCSANOW, &original_);
        }
#endif
    }

    int readKey() const {
#ifdef _WIN32
        if (_kbhit()) return _getch();
        return -1;
#else
        pollfd descriptor{STDIN_FILENO, POLLIN, 0};
        if (poll(&descriptor, 1, 0) > 0) {
            unsigned char value = 0;
            if (read(STDIN_FILENO, &value, 1) == 1) return value;
        }
        return -1;
#endif
    }

private:
#ifndef _WIN32
    termios original_{};
    bool active_ = false;
#endif
};

char rampChar(float t) {
    static constexpr char ramp[] = " .:-=+*#%@";
    const int index = std::max(
        0, std::min(9, static_cast<int>(std::lround(t * 9.0f))));
    return ramp[index];
}

void ansiMoveHome() { std::cout << "\x1b[H\x1b[2J"; }

std::string trimFloat(float value) {
    std::ostringstream stream;
    stream << std::fixed << std::setprecision(4) << value;
    return stream.str();
}

struct UiState {
    std::string message = "ready";
    terrain::SceneUpdateReport last_report;
    bool has_report = false;
};

void renderDashboard(const terrain::AsyncTerrainController& controller,
                     const std::vector<terrain::PatchBatch>& batches,
                     const UiState& state) {
    const terrain::TerrainScene& scene = controller.scene();
    const terrain::HeightField& height = scene.height();
    const terrain::SoftwareOffscreenRenderer& renderer = scene.renderer();
    ansiMoveHome();
    std::cout << "TERRAIN PATCH STUDIO  version=" << scene.version()
              << "  leaves=" << scene.quadtree().leaves().size()
              << "  block=" << scene.options().quadtree.block_size
              << "  threshold="
              << trimFloat(scene.options().quadtree.error_threshold)
              << "  busy=" << (controller.busy() ? "yes" : "no") << "\n";

    std::set<std::uint64_t> invalid;
    if (state.has_report) {
        invalid.insert(state.last_report.invalidation.invalid_blocks.begin(),
                       state.last_report.invalidation.invalid_blocks.end());
    }
    const std::uint32_t cols = 64;
    const std::uint32_t rows = 20;
    const float height_range =
        std::max(0.001f, height.valid_max - height.valid_min);
    std::cout << "ELEVATION HEAT  (red=invalidated block, + = leaf boundary)\n";
    for (std::uint32_t row = 0; row < rows; ++row) {
        for (std::uint32_t col = 0; col < cols; ++col) {
            const std::uint32_t x = static_cast<std::uint32_t>(
                (static_cast<double>(col) * (height.width - 1)) / (cols - 1));
            const std::uint32_t y = static_cast<std::uint32_t>(
                (static_cast<double>(row) * (height.height - 1)) / (rows - 1));
            bool is_invalid = false;
            bool boundary = false;
            for (std::uint64_t id : scene.quadtree().leaves()) {
                const terrain::QuadNode* node = scene.quadtree().node(id);
                if (node == nullptr) continue;
                if (node->rect.contains(x, y)) {
                    is_invalid = invalid.find(id) != invalid.end();
                    if (x == node->rect.x || x == node->rect.right() ||
                        y == node->rect.y || y == node->rect.bottom()) {
                        boundary = true;
                    }
                    break;
                }
            }
            const float normalized =
                std::max(0.0f, std::min(1.0f,
                                       (height.at(x, y) - height.valid_min) /
                                           height_range));
            const char c = boundary ? '+' : rampChar(normalized);
            if (is_invalid) {
                std::cout << "\x1b[31m" << c << "\x1b[0m";
            } else {
                std::cout << c;
            }
        }
        std::cout << "\n";
    }

    std::cout << "OFFSCREEN PREVIEW  " << renderer.lastSummary().color_hash
              << "\n";
    const terrain::OffscreenFrame& frame = renderer.frame();
    if (!frame.color.empty()) {
        const std::uint32_t preview_cols = 64;
        const std::uint32_t preview_rows = 12;
        for (std::uint32_t row = 0; row < preview_rows; ++row) {
            for (std::uint32_t col = 0; col < preview_cols; ++col) {
                const std::uint32_t px = static_cast<std::uint32_t>(
                    (static_cast<double>(col) * (frame.width - 1)) /
                    (preview_cols - 1));
                const std::uint32_t py = static_cast<std::uint32_t>(
                    (static_cast<double>(row) * (frame.height - 1)) /
                    (preview_rows - 1));
                const std::size_t index =
                    static_cast<std::size_t>(py) * frame.width + px;
                std::cout << "\x1b[48;2;" << int(frame.color[index * 4u]) << ";"
                          << int(frame.color[index * 4u + 1u]) << ";"
                          << int(frame.color[index * 4u + 2u]) << "m  ";
            }
            std::cout << "\x1b[0m\n";
        }
        std::cout << "ERROR HEAT BUFFER\n";
        const std::uint32_t heat_rows = 6;
        for (std::uint32_t row = 0; row < heat_rows; ++row) {
            for (std::uint32_t col = 0; col < preview_cols; ++col) {
                const std::uint32_t px = static_cast<std::uint32_t>(
                    (static_cast<double>(col) * (frame.width - 1)) /
                    (preview_cols - 1));
                const std::uint32_t py = static_cast<std::uint32_t>(
                    (static_cast<double>(row) * (frame.height - 1)) /
                    (heat_rows - 1));
                const std::size_t index =
                    static_cast<std::size_t>(py) * frame.width + px;
                const float heat =
                    static_cast<float>(frame.heat[index]) / 65535.0f;
                const int red = static_cast<int>(std::lround(heat * 255.0f));
                std::cout << "\x1b[38;2;" << red << ";"
                          << std::max(40, 220 - red) << ";40m"
                          << rampChar(heat);
            }
            std::cout << "\x1b[0m\n";
        }
    }

    std::cout << "PATCH BATCHES\n";
    for (const terrain::PatchBatch& batch : batches) {
        std::cout << "  " << (batch.sequence <= scene.version() ? "[done]    "
                                                                 : "[pending] ")
                  << "seq=" << batch.sequence
                  << (batch.height ? " height" : "")
                  << (batch.material ? " material" : "") << "\n";
    }
    if (state.has_report) {
        std::cout << "LAST: invalid="
                  << state.last_report.invalidation.invalid_blocks.size()
                  << " rerender="
                  << state.last_report.render.rerendered_blocks
                  << " reused=" << state.last_report.render.reused_blocks
                  << " seam=" << state.last_report.seams.max_deviation
                  << " normal="
                  << state.last_report.surface.max_normal_angle_deg
                  << " render_equal="
                  << (state.last_report.full_incremental_equal ? "yes" : "no")
                  << "\n";
        if (!state.last_report.invalidation.invalid_blocks.empty()) {
            const std::uint64_t first =
                state.last_report.invalidation.invalid_blocks.front();
            std::cout << "WHY: "
                      << state.last_report.invalidation.reasonFor(first)
                      << "\n";
        }
    }
    std::cout << "STATUS: " << state.message << "\n"
              << "[a] apply next  [c] cancel  [+/-] block size  "
                 "[/] threshold  [q] quit\n"
              << std::flush;
}

terrain::PatchBatch* nextPending(
    std::vector<terrain::PatchBatch>& batches, std::uint64_t version) {
    for (terrain::PatchBatch& batch : batches) {
        if (batch.sequence == version + 1u) return &batch;
    }
    return nullptr;
}

}  // namespace

int main(int argc, char** argv) {
    std::string samples_root = argc >= 2 ? argv[1] : "samples";
    const bool once =
        argc >= 3 && std::string(argv[2]) == "--once";
    terrain::SamplePaths paths;
    terrain::Status status =
        terrain::generateDeterministicSamples(samples_root, &paths);
    if (!status.ok()) {
        std::cerr << status.toString() << "\n";
        return 1;
    }
    terrain::VersionStore store(paths.store_directory);
    status = store.initializeFromFiles(paths.height_field,
                                       paths.material_image);
    if (!status.ok()) {
        status = store.loadCurrent(nullptr, nullptr, nullptr);
        if (!status.ok()) {
            std::cerr << status.toString() << "\n";
            return 1;
        }
    }
    terrain::AsyncTerrainController controller;
    terrain::SceneOptions options;
    status = controller.initializeFromStore(&store, options);
    if (!status.ok()) {
        std::cerr << status.toString() << "\n";
        return 1;
    }
    std::vector<terrain::PatchBatch> batches;
    status = terrain::groupPatchDirectory(paths.patch_directory, &batches);
    if (!status.ok()) {
        std::cerr << status.toString() << "\n";
        return 1;
    }
    UiState state;
    state.message =
        "loaded version " + std::to_string(controller.scene().version());
    if (once) {
        renderDashboard(controller, batches, state);
        return 0;
    }

    TerminalMode terminal;
    auto last_draw = std::chrono::steady_clock::now();
    renderDashboard(controller, batches, state);
    bool running = true;
    while (running) {
        terrain::JobResult result;
        if (controller.busy() && controller.waitForCompletion(0, &result)) {
            if (result.completed) {
                if (result.report.patch_sequence != 0 ||
                    !result.report.notes.empty()) {
                    state.last_report = result.report;
                    state.has_report = true;
                }
                state.message = result.status.toString();
                if (result.status.ok() && result.report.patch_sequence != 0) {
                    state.message +=
                        "  version=" +
                        std::to_string(controller.scene().version());
                }
                last_draw = std::chrono::steady_clock::now();
            }
        }
        const int key = terminal.readKey();
        if (key >= 0) {
            const char command = static_cast<char>(key);
            if (command == 'q' || command == 'Q') {
                running = false;
            } else if (command == 'a' || command == 'A') {
                if (controller.busy()) {
                    state.message = "E_BUSY: one recompute task is active";
                } else if (terrain::PatchBatch* next =
                               nextPending(batches,
                                           controller.scene().version())) {
                    status = controller.submit(*next, &store);
                    state.message = status.ok()
                                        ? "applying patch " +
                                              std::to_string(next->sequence) +
                                              " in background"
                                        : status.toString();
                } else {
                    state.message = "no pending patch batch";
                }
            } else if (command == 'c' || command == 'C') {
                status = controller.cancel();
                state.message = status.ok() ? "cancel requested"
                                            : status.toString();
            } else if ((command == '+' || command == '-') &&
                       !controller.busy()) {
                terrain::SceneOptions next = controller.scene().options();
                const int delta = command == '+' ? 8 : -8;
                const int value =
                    static_cast<int>(next.quadtree.block_size) + delta;
                next.quadtree.block_size = static_cast<std::uint32_t>(
                    std::max(16, std::min(64, value)));
                status = controller.submitOptions(next);
                state.message =
                    status.ok() ? "rebuilding block hierarchy" : status.toString();
            } else if ((command == '[' || command == ']') &&
                       !controller.busy()) {
                terrain::SceneOptions next = controller.scene().options();
                const float delta = command == ']' ? 0.5f : -0.5f;
                next.quadtree.error_threshold =
                    std::max(0.5f, next.quadtree.error_threshold + delta);
                status = controller.submitOptions(next);
                state.message =
                    status.ok() ? "applying new error threshold"
                                : status.toString();
            }
            last_draw = std::chrono::steady_clock::now();
        }
        const auto now = std::chrono::steady_clock::now();
        if (!controller.busy() &&
            now - last_draw > std::chrono::milliseconds(180)) {
            renderDashboard(controller, batches, state);
            last_draw = now;
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(25));
    }
    controller.cancel();
    return 0;
}

