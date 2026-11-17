cmake_minimum_required(VERSION 3.21)

project(church_mosaic_validator LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(mosaic_core
    src/json.cpp
    src/sha256.cpp
    src/scene.cpp
    src/render.cpp
    src/snapshot.cpp
    src/png.cpp
)

target_include_directories(mosaic_core PUBLIC src)

if(MSVC)
    target_compile_options(mosaic_core PUBLIC /W4 /permissive-)
else()
    target_compile_options(mosaic_core PUBLIC -Wall -Wextra -Wpedantic)
endif()

add_executable(mosaic-validator src/main.cpp)
target_link_libraries(mosaic-validator PRIVATE mosaic_core)

enable_testing()

add_executable(mosaic_core_tests tests/test_core.cpp)
target_link_libraries(mosaic_core_tests PRIVATE mosaic_core)
add_test(NAME mosaic_core_tests COMMAND mosaic_core_tests)
