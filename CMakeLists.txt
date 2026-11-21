cmake_minimum_required(VERSION 3.20)

project(chunk_preview
    VERSION 1.0.0
    DESCRIPTION "Deterministic streaming chunk preview and occlusion error-budget module"
    LANGUAGES CXX)

option(CHUNK_PREVIEW_BUILD_TESTS "Build local acceptance tests" ON)
option(CHUNK_PREVIEW_BUILD_TOOLS "Build sample generator and CLI" ON)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(Threads REQUIRED)

add_library(chunk_preview_core STATIC
    src/error.cpp
    src/hash.cpp
    src/math.cpp
    src/scene.cpp
    src/scene_io.cpp
    src/streaming.cpp
    src/culling.cpp
    src/depth_pyramid.cpp
    src/occlusion.cpp
    src/error_model.cpp
    src/visibility.cpp
    src/snapshot.cpp
    src/renderer.cpp
    src/sample_scene.cpp
)

target_include_directories(chunk_preview_core
    PUBLIC
        ${CMAKE_CURRENT_SOURCE_DIR}/include
)

target_link_libraries(chunk_preview_core PUBLIC Threads::Threads)

if(MSVC)
    target_compile_options(chunk_preview_core PRIVATE /W4 /permissive-)
else()
    target_compile_options(chunk_preview_core PRIVATE
        -Wall -Wextra -Wpedantic -Wconversion -Wshadow
        -Wno-sign-conversion)
endif()

if(CHUNK_PREVIEW_BUILD_TOOLS)
    add_executable(scene_gen tools/scene_gen.cpp)
    target_link_libraries(scene_gen PRIVATE chunk_preview_core)

    add_executable(preview_cli tools/preview_cli.cpp)
    target_link_libraries(preview_cli PRIVATE chunk_preview_core)
endif()

if(CHUNK_PREVIEW_BUILD_TESTS)
    enable_testing()

    add_executable(test_scene tests/test_scene.cpp)
    target_link_libraries(test_scene PRIVATE chunk_preview_core)
    add_test(NAME scene_format_and_validation COMMAND test_scene)

    add_executable(test_streaming tests/test_streaming.cpp)
    target_link_libraries(test_streaming PRIVATE chunk_preview_core)
    add_test(NAME streaming_snapshots COMMAND test_streaming)

    add_executable(test_render tests/test_render.cpp)
    target_link_libraries(test_render PRIVATE chunk_preview_core)
    add_test(NAME occlusion_error_and_determinism COMMAND test_render)

    add_executable(test_snapshot tests/test_snapshot.cpp)
    target_link_libraries(test_snapshot PRIVATE chunk_preview_core)
    add_test(NAME snapshot_recovery COMMAND test_snapshot)
endif()
