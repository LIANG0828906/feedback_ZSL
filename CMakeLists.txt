cmake_minimum_required(VERSION 3.20)

project(shadowpipe VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(Threads REQUIRED)

add_library(shadowpipe_core
    src/config.cpp
    src/hash.cpp
    src/raster.cpp
    src/scene.cpp
    src/synthetic.cpp
    src/cascades.cpp
    src/pyramid.cpp
    src/renderer.cpp
    src/snapshot.cpp
)

target_include_directories(shadowpipe_core
    PUBLIC
        ${CMAKE_CURRENT_SOURCE_DIR}/include
        ${CMAKE_CURRENT_SOURCE_DIR}/third_party
)

target_link_libraries(shadowpipe_core PUBLIC Threads::Threads)

if(MSVC)
    target_compile_options(shadowpipe_core PRIVATE /W4 /permissive-)
else()
    target_compile_options(shadowpipe_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(scene_gen apps/scene_gen.cpp)
target_link_libraries(scene_gen PRIVATE shadowpipe_core)

add_executable(shadowpipe apps/shadowpipe_cli.cpp)
target_link_libraries(shadowpipe PRIVATE shadowpipe_core)

enable_testing()
add_executable(shadowpipe_tests tests/tests.cpp)
target_link_libraries(shadowpipe_tests PRIVATE shadowpipe_core)
add_test(NAME shadowpipe_core COMMAND shadowpipe_tests)
add_custom_target(check
    COMMAND shadowpipe_tests
    DEPENDS shadowpipe_tests
    COMMENT "Running deterministic shadowpipe acceptance tests"
)

add_custom_target(samples
    COMMAND scene_gen --out-dir ${CMAKE_CURRENT_SOURCE_DIR}/data
    DEPENDS scene_gen
    COMMENT "Generating deterministic synthetic scenes under data/"
)

