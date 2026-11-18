cmake_minimum_required(VERSION 3.16)
project(silklight_validation LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_executable(silklight
    src/main.cpp
    src/json.cpp
    src/scene.cpp
    src/halftone.cpp
    src/render.cpp
    src/snapshot.cpp
    src/png.cpp
)

target_include_directories(silklight PRIVATE src)

if(MSVC)
    target_compile_options(silklight PRIVATE /W4 /permissive- /utf-8)
else()
    target_compile_options(silklight PRIVATE -Wall -Wextra -Wpedantic)
endif()

find_package(OpenMP QUIET)
if(OpenMP_CXX_FOUND)
    target_link_libraries(silklight PRIVATE OpenMP::OpenMP_CXX)
    target_compile_definitions(silklight PRIVATE SILKLIGHT_HAS_OPENMP=1)
endif()

include(CTest)
if(BUILD_TESTING)
    add_test(NAME silklight_self_test
             COMMAND silklight self-test --work-dir "${CMAKE_BINARY_DIR}/self-test")
    find_package(Python3 COMPONENTS Interpreter QUIET)
    if(Python3_Interpreter_FOUND AND UNIX)
        add_test(NAME silklight_external_interrupt
                 COMMAND "${Python3_EXECUTABLE}" "${CMAKE_SOURCE_DIR}/tests/integration.py"
                         --exe "$<TARGET_FILE:silklight>"
                         --work-dir "${CMAKE_BINARY_DIR}/integration-test")
    endif()
endif()

