# Offline OpenGL Dependencies

The default build and acceptance tests do not require OpenGL, GLFW or glad.
The deterministic software renderer is the reference backend.

For the optional OpenGL 3.3 preview, place locally downloaded/build sources in
one of these layouts:

```text
third_party/
  glfw/
    include/GLFW/glfw3.h
    lib/...
  glad/
    include/glad/glad.h
    include/KHR/khrplatform.h
    src/glad.c
```

Configure with:

```bash
cmake -S . -B build \
  -DTERRAIN_ENABLE_GLFW=ON \
  -DTERRAIN_GLFW_ROOT="$PWD/third_party/glfw" \
  -DTERRAIN_GLAD_ROOT="$PWD/third_party/glad"
```

CMake only searches these local paths and installed local packages. It does not
use `FetchContent`, git clones, downloads or any network service.

