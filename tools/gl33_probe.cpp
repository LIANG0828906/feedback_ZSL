#include <cstdio>
#include <cstdlib>
#include <dlfcn.h>

namespace {

using EGLDisplay = void*;
using EGLContext = void*;
using EGLSurface = void*;
using EGLConfig = void*;
using EGLBoolean = unsigned int;
using EGLint = int;

constexpr unsigned int kEglPlatformSurfacelessMesa = 0x31DD;
constexpr unsigned int kEglOpenGlApi = 0x30A2;
constexpr EGLint kEglContextMajorVersion = 0x3098;
constexpr EGLint kEglContextMinorVersion = 0x30FB;
constexpr EGLint kEglContextOpenGlProfileMask = 0x30FD;
constexpr EGLint kEglContextOpenGlCoreProfileBit = 0x0001;
constexpr EGLint kEglNone = 0x3038;
constexpr unsigned int kGlVendor = 0x1F00;
constexpr unsigned int kGlRenderer = 0x1F01;
constexpr unsigned int kGlVersion = 0x1F02;

using EglGetPlatformDisplay = EGLDisplay (*)(unsigned int, void*, const EGLint*);
using EglInitialize = EGLBoolean (*)(EGLDisplay, EGLint*, EGLint*);
using EglBindApi = EGLBoolean (*)(unsigned int);
using EglCreateContext =
    EGLContext (*)(EGLDisplay, EGLConfig, EGLContext, const EGLint*);
using EglMakeCurrent =
    EGLBoolean (*)(EGLDisplay, EGLSurface, EGLSurface, EGLContext);
using EglGetProcAddress = void* (*)(const char*);
using GlGetString = const unsigned char* (*)(unsigned int);

template <typename Function>
Function load(void* library, const char* name) {
    return reinterpret_cast<Function>(dlsym(library, name));
}

}  // namespace

int main() {
    void* egl = dlopen("libEGL.so.1", RTLD_NOW);
    if (egl == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: cannot load libEGL.so.1\n");
        return 1;
    }
    const auto get_display =
        load<EglGetPlatformDisplay>(egl, "eglGetPlatformDisplay");
    const auto initialize = load<EglInitialize>(egl, "eglInitialize");
    const auto bind_api = load<EglBindApi>(egl, "eglBindAPI");
    const auto create_context = load<EglCreateContext>(egl, "eglCreateContext");
    const auto make_current = load<EglMakeCurrent>(egl, "eglMakeCurrent");
    const auto get_proc_address =
        load<EglGetProcAddress>(egl, "eglGetProcAddress");
    if (get_display == nullptr || initialize == nullptr || bind_api == nullptr ||
        create_context == nullptr || make_current == nullptr ||
        get_proc_address == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: missing EGL entry points\n");
        return 1;
    }
    EGLDisplay display = get_display(kEglPlatformSurfacelessMesa, nullptr, nullptr);
    if (display == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: no surfaceless EGL display\n");
        return 1;
    }
    EGLint major = 0;
    EGLint minor = 0;
    if (initialize(display, &major, &minor) == 0U) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: eglInitialize failed\n");
        return 1;
    }
    if (bind_api(kEglOpenGlApi) == 0U) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: cannot bind OpenGL API\n");
        return 1;
    }
    const EGLint context_attributes[] = {
        kEglContextMajorVersion, 3,
        kEglContextMinorVersion, 3,
        kEglContextOpenGlProfileMask, kEglContextOpenGlCoreProfileBit,
        kEglNone};
    EGLContext context = create_context(
        display, nullptr, nullptr, context_attributes);
    if (context == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: create 3.3 context failed\n");
        return 1;
    }
    if (make_current(display, nullptr, nullptr, context) == 0U) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: surfaceless make-current failed\n");
        return 1;
    }
    const auto gl_get_string =
        reinterpret_cast<GlGetString>(get_proc_address("glGetString"));
    if (gl_get_string == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: glGetString unavailable\n");
        return 1;
    }
    const char* vendor =
        reinterpret_cast<const char*>(gl_get_string(kGlVendor));
    const char* renderer =
        reinterpret_cast<const char*>(gl_get_string(kGlRenderer));
    const char* version =
        reinterpret_cast<const char*>(gl_get_string(kGlVersion));
    if (version == nullptr) {
        std::fprintf(stderr, "OPENGL_PROBE_FAIL: empty GL_VERSION\n");
        return 1;
    }
    int gl_major = 0;
    int gl_minor = 0;
    std::sscanf(version, "%d.%d", &gl_major, &gl_minor);
    std::printf("vendor=%s\nrenderer=%s\nversion=%s\n",
                vendor != nullptr ? vendor : "?",
                renderer != nullptr ? renderer : "?",
                version);
    if (gl_major > 3 || (gl_major == 3 && gl_minor >= 3)) {
        std::printf("OPENGL_3_3_OK\n");
        return 0;
    }
    std::fprintf(stderr, "OPENGL_PROBE_FAIL: context is below 3.3\n");
    return 1;
}
