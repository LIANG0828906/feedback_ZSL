cmake_minimum_required(VERSION 3.20)

project(battle_pick LANGUAGES CXX VERSION 1.0.0)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)
set(CMAKE_EXPORT_COMPILE_COMMANDS ON)

option(BATTLE_BUILD_TESTS "Build the deterministic battle-pick test suite" ON)
option(BATTLE_ENABLE_SANITIZERS "Enable address and undefined-behavior sanitizers" OFF)
option(BATTLE_BUILD_GL_PROBE "Build the Linux EGL surfaceless OpenGL probe" OFF)

find_package(Threads REQUIRED)

# GLM 1.0.1 is vendored for offline, reproducible builds. The fallback is still
# pinned by version and archive hash for a clean source checkout.
set(BATTLE_GLM_VERSION "1.0.1")
if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/third_party/glm/CMakeLists.txt")
    add_subdirectory(third_party/glm EXCLUDE_FROM_ALL)
else()
    include(FetchContent)
    FetchContent_Declare(
        glm
        URL "https://github.com/g-truc/glm/archive/refs/tags/1.0.1.tar.gz"
        URL_HASH "SHA256=9f3174561fd26904b23f0db5e560971cbf9b3cbda0b280f04d5c379d03bf234c"
    )
    FetchContent_MakeAvailable(glm)
endif()

find_package(OpenGL QUIET)
if(OpenGL_FOUND)
    message(STATUS "OpenGL libraries found (headless core does not require a GL context)")
else()
    message(STATUS "OpenGL libraries not found; building the headless deterministic core")
endif()

add_library(battle_core
    src/sha256.cpp
    src/scene.cpp
    src/region.cpp
    src/bvh.cpp
    src/camera.cpp
    src/query.cpp
    src/placement.cpp
    src/trace.cpp
    src/injection.cpp
    src/sample.cpp
)

target_include_directories(battle_core
    PUBLIC
        "${CMAKE_CURRENT_SOURCE_DIR}/include"
)

target_link_libraries(battle_core
    PUBLIC
        glm::glm
        Threads::Threads
)

if(MSVC)
    target_compile_options(battle_core PRIVATE /W4 /permissive- /fp:precise /Zc:preprocessor)
    target_compile_definitions(battle_core PRIVATE _CRT_SECURE_NO_WARNINGS NOMINMAX)
else()
    target_compile_options(battle_core PRIVATE
        -Wall -Wextra -Wpedantic -Wconversion -Wshadow
        -ffp-contract=off -fno-fast-math
    )
endif()

if(BATTLE_ENABLE_SANITIZERS AND NOT MSVC)
    target_compile_options(battle_core PUBLIC -fsanitize=address,undefined -fno-omit-frame-pointer)
    target_link_options(battle_core PUBLIC -fsanitize=address,undefined)
endif()

add_executable(battle_cli src/cli_main.cpp)
target_link_libraries(battle_cli PRIVATE battle_core)
if(MSVC)
    target_compile_options(battle_cli PRIVATE /W4 /permissive- /fp:precise)
else()
    target_compile_options(battle_cli PRIVATE -Wall -Wextra -Wpedantic -ffp-contract=off -fno-fast-math)
endif()

if(BATTLE_BUILD_TESTS)
    include(CTest)
    add_executable(battle_tests tests/test_main.cpp)
    target_link_libraries(battle_tests PRIVATE battle_core)
    if(MSVC)
        target_compile_options(battle_tests PRIVATE /W4 /permissive- /fp:precise)
    else()
        target_compile_options(battle_tests PRIVATE -Wall -Wextra -Wpedantic -ffp-contract=off -fno-fast-math)
    endif()
    add_test(NAME battle_unit_and_integration
        COMMAND battle_tests "${CMAKE_CURRENT_BINARY_DIR}/test-work")
    set_tests_properties(battle_unit_and_integration PROPERTIES TIMEOUT 180)
endif()

install(TARGETS battle_cli RUNTIME DESTINATION bin)

if(BATTLE_BUILD_GL_PROBE AND UNIX AND NOT APPLE)
    add_executable(gl33_probe tools/gl33_probe.cpp)
    target_link_libraries(gl33_probe PRIVATE ${CMAKE_DL_LIBS})
    if(NOT MSVC)
        target_compile_options(gl33_probe PRIVATE -Wall -Wextra -Wpedantic)
    endif()
    install(TARGETS gl33_probe RUNTIME DESTINATION bin)
endif()

