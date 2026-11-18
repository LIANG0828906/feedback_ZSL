cmake_minimum_required(VERSION 3.16)

project(coffered_ceiling_soft_raster LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(COFFERED_BUILD_TESTS "Build and register deterministic validation tests" ON)

add_executable(coffered_raster
    src/main.cpp
    src/json.cpp
    src/hash.cpp
    src/scene.cpp
    src/texture.cpp
    src/render.cpp
    src/output.cpp
    src/snapshot.cpp
)

add_executable(coffered_texture_probe
    src/texture_probe.cpp
    src/texture.cpp
    src/hash.cpp
    src/json.cpp
    src/output.cpp
)

target_include_directories(coffered_raster PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/include
    ${CMAKE_CURRENT_SOURCE_DIR}/src
)

target_include_directories(coffered_texture_probe PRIVATE
    ${CMAKE_CURRENT_SOURCE_DIR}/include
    ${CMAKE_CURRENT_SOURCE_DIR}/src
)

if(MSVC)
    target_compile_options(coffered_raster PRIVATE /W4 /permissive- /utf-8)
    target_compile_definitions(coffered_raster PRIVATE _CRT_SECURE_NO_WARNINGS NOMINMAX)
    target_compile_options(coffered_texture_probe PRIVATE /W4 /permissive- /utf-8)
    target_compile_definitions(coffered_texture_probe PRIVATE _CRT_SECURE_NO_WARNINGS NOMINMAX)
else()
    target_compile_options(coffered_raster PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wno-sign-conversion)
    target_compile_options(coffered_texture_probe PRIVATE -Wall -Wextra -Wpedantic -Wconversion -Wno-sign-conversion)
    set_source_files_properties(src/output.cpp PROPERTIES COMPILE_OPTIONS "-Wno-conversion")
endif()

if(WIN32)
    target_link_libraries(coffered_raster PRIVATE kernel32)
endif()

if(COFFERED_BUILD_TESTS)
    include(CTest)
    find_package(Python3 COMPONENTS Interpreter QUIET)
    if(Python3_Interpreter_FOUND)
        add_test(
            NAME deterministic_validation
            COMMAND ${Python3_EXECUTABLE}
                    ${CMAKE_CURRENT_SOURCE_DIR}/tests/run_tests.py
                    --exe $<TARGET_FILE:coffered_raster>
                    --texture-probe $<TARGET_FILE:coffered_texture_probe>
                    --work ${CMAKE_CURRENT_BINARY_DIR}/validation-work
        )
        set_tests_properties(deterministic_validation PROPERTIES TIMEOUT 180)
    endif()
endif()

