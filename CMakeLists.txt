cmake_minimum_required(VERSION 3.20)

project(softracer LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_executable(softracer
    src/main.cpp
    src/json.cpp
    src/sha256.cpp
    src/scene.cpp
    src/renderer.cpp
    src/image.cpp
    src/snapshot.cpp
    src/report.cpp
    src/util.cpp
)

target_include_directories(softracer PRIVATE src)
target_compile_definitions(softracer PRIVATE SOFTRACER_VERSION="1.0.0")

if(MSVC)
    target_compile_options(softracer PRIVATE /W4 /permissive- /utf-8 /EHsc)
else()
    target_compile_options(softracer PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wshadow)
endif()

include(CTest)
find_package(Python3 COMPONENTS Interpreter QUIET)
if(BUILD_TESTING AND Python3_Interpreter_FOUND)
    add_test(
        NAME softracer_acceptance
        COMMAND "${Python3_EXECUTABLE}" "${CMAKE_CURRENT_SOURCE_DIR}/tests/run_tests.py"
                --binary "$<TARGET_FILE:softracer>"
                --work "${CMAKE_CURRENT_BINARY_DIR}/acceptance"
    )
    set_tests_properties(softracer_acceptance PROPERTIES TIMEOUT 240)
endif()

