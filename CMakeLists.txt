cmake_minimum_required(VERSION 3.20)

project(dunhuang_caisson LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

find_package(PNG 1.6 REQUIRED)

add_executable(caisson_render
    src/main.cpp
    src/json.cpp
    src/sha256.cpp
    src/util.cpp
    src/scene.cpp
    src/texture.cpp
    src/png_io.cpp
    src/renderer.cpp
    src/shadow.cpp
    src/snapshot.cpp
    src/report.cpp
)

target_include_directories(caisson_render PRIVATE src)
target_link_libraries(caisson_render PRIVATE PNG::PNG)

if(MSVC)
    target_compile_options(caisson_render PRIVATE /W4 /permissive- /utf-8)
else()
    target_compile_options(caisson_render PRIVATE -Wall -Wextra -Wpedantic)
endif()

enable_testing()

find_package(Python3 3.10 COMPONENTS Interpreter)
if(Python3_Interpreter_FOUND)
    add_test(
        NAME caisson_integration
        COMMAND "${Python3_EXECUTABLE}" "${CMAKE_CURRENT_SOURCE_DIR}/tests/integration_test.py"
                --exe "$<TARGET_FILE:caisson_render>"
                --work-dir "${CMAKE_CURRENT_BINARY_DIR}/integration_work"
    )
    set_tests_properties(caisson_integration PROPERTIES TIMEOUT 900)
endif()

