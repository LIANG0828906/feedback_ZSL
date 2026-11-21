#include "meshrepair/api.hpp"

#if defined(MESHREPAIR_HAS_UI)

#include <GLFW/glfw3.h>

#include <algorithm>
#include <atomic>
#include <cmath>
#include <cstdlib>
#include <iostream>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>

namespace {

struct UiState {
    std::mutex mutex;
    std::shared_ptr<const meshrepair::Snapshot> snapshot;
    std::shared_ptr<const meshrepair::RepairReport> report;
    meshrepair::Error error;
    std::string status = "starting";
    bool done = false;
};

std::string option(
    const std::map<std::string, std::string>& values,
    const std::string& name,
    const std::string& fallback = {}) {
    auto found = values.find(name);
    return found == values.end() ? fallback : found->second;
}

struct Bounds {
    meshrepair::Vec3 minimum;
    meshrepair::Vec3 maximum;
    meshrepair::Vec3 center;
    double radius = 1.0;
};

Bounds boundsOf(const meshrepair::Mesh& mesh) {
    Bounds bounds;
    if (mesh.vertices.empty()) return bounds;
    bounds.minimum = mesh.vertices.front();
    bounds.maximum = mesh.vertices.front();
    for (const meshrepair::Vec3& p : mesh.vertices) {
        bounds.minimum.x = std::min(bounds.minimum.x, p.x);
        bounds.minimum.y = std::min(bounds.minimum.y, p.y);
        bounds.minimum.z = std::min(bounds.minimum.z, p.z);
        bounds.maximum.x = std::max(bounds.maximum.x, p.x);
        bounds.maximum.y = std::max(bounds.maximum.y, p.y);
        bounds.maximum.z = std::max(bounds.maximum.z, p.z);
    }
    bounds.center = {
        (bounds.minimum.x + bounds.maximum.x) * 0.5,
        (bounds.minimum.y + bounds.maximum.y) * 0.5,
        (bounds.minimum.z + bounds.maximum.z) * 0.5};
    const double dx = bounds.maximum.x - bounds.minimum.x;
    const double dy = bounds.maximum.y - bounds.minimum.y;
    const double dz = bounds.maximum.z - bounds.minimum.z;
    bounds.radius = std::max(1e-6, std::sqrt(dx * dx + dy * dy + dz * dz) * 0.75);
    return bounds;
}

void chunkColor(uint32_t chunk, float& r, float& g, float& b) {
    static const float palette[8][3] = {
        {0.92f, 0.32f, 0.24f},
        {0.20f, 0.70f, 0.42f},
        {0.22f, 0.48f, 0.92f},
        {0.95f, 0.68f, 0.16f},
        {0.66f, 0.33f, 0.86f},
        {0.12f, 0.78f, 0.82f},
        {0.90f, 0.42f, 0.68f},
        {0.55f, 0.78f, 0.20f}};
    const float* color = palette[chunk % 8];
    r = color[0];
    g = color[1];
    b = color[2];
}

void drawSnapshot(
    const meshrepair::Snapshot& snapshot,
    bool wireframe,
    double angle) {
    const Bounds bounds = boundsOf(snapshot.mesh);
    int width = 1;
    int height = 1;
    glfwGetFramebufferSize(glfwGetCurrentContext(), &width, &height);
    if (height <= 0) height = 1;
    const double aspect = static_cast<double>(width) / static_cast<double>(height);

    glViewport(0, 0, width, height);
    glClearColor(0.06f, 0.07f, 0.09f, 1.0f);
    glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
    glEnable(GL_DEPTH_TEST);
    glMatrixMode(GL_PROJECTION);
    glLoadIdentity();
    const double extent = bounds.radius * 1.25;
    if (aspect >= 1.0) {
        glOrtho(
            -extent * aspect, extent * aspect, -extent, extent,
            -extent * 10.0, extent * 10.0);
    } else {
        glOrtho(
            -extent, extent, -extent / aspect, extent / aspect,
            -extent * 10.0, extent * 10.0);
    }
    glMatrixMode(GL_MODELVIEW);
    glLoadIdentity();
    glTranslated(0.0, 0.0, -bounds.radius * 4.0);
    glRotated(24.0, 1.0, 0.0, 0.0);
    glRotated(angle, 0.0, 1.0, 0.0);
    glTranslated(-bounds.center.x, -bounds.center.y, -bounds.center.z);

    glPolygonMode(GL_FRONT_AND_BACK, wireframe ? GL_LINE : GL_FILL);
    glBegin(GL_TRIANGLES);
    for (size_t i = 0; i < snapshot.mesh.faces.size(); ++i) {
        const uint32_t chunk =
            i < snapshot.face_chunk.size() ? snapshot.face_chunk[i] : 0;
        float r = 0.8f;
        float g = 0.8f;
        float b = 0.8f;
        chunkColor(chunk, r, g, b);
        glColor3f(r, g, b);
        const meshrepair::Face& face = snapshot.mesh.faces[i];
        for (uint64_t index : face.v) {
            const meshrepair::Vec3& p =
                snapshot.mesh.vertices[static_cast<size_t>(index)];
            glVertex3d(p.x, p.y, p.z);
        }
    }
    glEnd();
    glPolygonMode(GL_FRONT_AND_BACK, GL_FILL);
}

}  // namespace

int main(int argc, char** argv) {
    std::map<std::string, std::string> values;
    for (int i = 1; i < argc; ++i) {
        const std::string token = argv[i];
        if (token.rfind("--", 0) == 0 && i + 1 < argc) {
            values[token.substr(2)] = argv[++i];
        }
    }
    const std::string input = option(values, "input");
    const std::string config_path = option(values, "config");
    const std::string output = option(values, "out");
    uint64_t frame_limit = 0;
    if (!option(values, "frames").empty()) {
        try {
            frame_limit = std::stoull(option(values, "frames"));
        } catch (...) {
            std::cerr << "error: --frames requires an integer\n";
            return 64;
        }
    }
    if (input.empty() || config_path.empty() || output.empty()) {
        std::cerr
            << "usage: meshrepair_ui --input sample.mesh --config sample.cfg "
               "--out run-dir\n";
        return 64;
    }

    if (!glfwInit()) {
        std::cerr << "error: GLFW initialization failed\n";
        return 2;
    }
    glfwWindowHint(GLFW_CONTEXT_VERSION_MAJOR, 2);
    glfwWindowHint(GLFW_CONTEXT_VERSION_MINOR, 1);
    GLFWwindow* window = glfwCreateWindow(
        1100, 760, "MeshRepair committed snapshot preview", nullptr, nullptr);
    if (window == nullptr) {
        std::cerr << "error: cannot create OpenGL window\n";
        glfwTerminate();
        return 2;
    }
    glfwMakeContextCurrent(window);
    glfwSwapInterval(1);

    auto state = std::make_shared<UiState>();
    std::atomic<bool> cancel{false};
    std::thread worker([state, input, config_path, output, &cancel]() {
        auto mesh = meshrepair::loadMesh(input);
        if (!mesh) {
            std::lock_guard<std::mutex> lock(state->mutex);
            state->error = mesh.error;
            state->status = "load failed";
            state->done = true;
            return;
        }
        auto config = meshrepair::loadConfig(config_path);
        if (!config) {
            std::lock_guard<std::mutex> lock(state->mutex);
            state->error = config.error;
            state->status = "config failed";
            state->done = true;
            return;
        }
        meshrepair::RepairOptions options;
        options.cancel = &cancel;
        options.on_snapshot = [state](std::shared_ptr<const meshrepair::Snapshot> snapshot) {
            std::lock_guard<std::mutex> lock(state->mutex);
            state->snapshot = std::move(snapshot);
            state->status = "committed snapshot";
        };
        auto result =
            meshrepair::repairMesh(*mesh, *config, output, options);
        std::lock_guard<std::mutex> lock(state->mutex);
        if (result) {
            state->report = std::make_shared<meshrepair::RepairReport>(*result);
            state->status = "complete";
        } else {
            state->error = result.error;
            state->status = meshrepair::errorCodeName(result.error.code);
        }
        state->done = true;
    });

    bool wireframe = false;
    bool w_pressed = false;
    double angle = 0.0;
    uint64_t frame_number = 0;
    std::string last_title;
    while (!glfwWindowShouldClose(window)) {
        glfwPollEvents();
        const bool escape =
            glfwGetKey(window, GLFW_KEY_ESCAPE) == GLFW_PRESS;
        if (escape) cancel.store(true, std::memory_order_relaxed);
        const bool w_down = glfwGetKey(window, GLFW_KEY_W) == GLFW_PRESS;
        if (w_down && !w_pressed) wireframe = !wireframe;
        w_pressed = w_down;

        std::shared_ptr<const meshrepair::Snapshot> snapshot;
        std::string status;
        bool done = false;
        bool have_error = false;
        {
            std::lock_guard<std::mutex> lock(state->mutex);
            snapshot = state->snapshot;
            status = state->status;
            done = state->done;
            have_error = !state->error.ok();
        }
        if (snapshot) {
            drawSnapshot(*snapshot, wireframe, angle);
        } else {
            glViewport(0, 0, 1100, 760);
            glClearColor(0.06f, 0.07f, 0.09f, 1.0f);
            glClear(GL_COLOR_BUFFER_BIT | GL_DEPTH_BUFFER_BIT);
        }
        glfwSwapBuffers(window);
        if (!wireframe) angle += 0.15;

        std::string title = "MeshRepair | " + status;
        if (snapshot) {
            title += " | chunk " + std::to_string(snapshot->committed_count);
            title += " | " + snapshot->hash.substr(0, 12);
        }
        if (done && have_error) title += " | press Esc to close";
        if (title != last_title) {
            glfwSetWindowTitle(window, title.c_str());
            last_title = title;
        }
        if (done && cancel.load(std::memory_order_relaxed) &&
            glfwGetKey(window, GLFW_KEY_ESCAPE) == GLFW_RELEASE) {
            // Keep the last committed snapshot visible; the user closes the
            // window when finished.
        }
        ++frame_number;
        if (frame_limit != 0 && frame_number >= frame_limit) {
            glfwSetWindowShouldClose(window, GLFW_TRUE);
        }
    }
    cancel.store(true, std::memory_order_relaxed);
    if (worker.joinable()) worker.join();
    glfwDestroyWindow(window);
    glfwTerminate();
    return 0;
}

#else

#include <iostream>

int main() {
    std::cerr << "meshrepair_ui was built without GLFW/OpenGL support\n";
    return 2;
}

#endif
