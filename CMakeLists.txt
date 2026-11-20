cmake_minimum_required(VERSION 3.20)

project(fill_pipeline VERSION 1.0.0 LANGUAGES CXX)

option(FILL_PIPELINE_BUILD_TESTS "Build deterministic unit tests" ON)
option(FILL_PIPELINE_BUILD_QT_PREVIEW
       "Build the optional Qt 5/6 image preview tool" OFF)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

add_library(fill_core
    src/json.cpp
    src/sha256.cpp
    src/path.cpp
    src/geometry.cpp
    src/engine.cpp
    src/pipeline.cpp
)
target_include_directories(fill_core PUBLIC ${CMAKE_CURRENT_SOURCE_DIR}/src)

if(MSVC)
    target_compile_options(fill_core PRIVATE /W4 /permissive- /utf-8)
else()
    target_compile_options(fill_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(fill_pipeline src/main.cpp)
target_link_libraries(fill_pipeline PRIVATE fill_core)

if(FILL_PIPELINE_BUILD_QT_PREVIEW)
    find_package(Qt6 QUIET COMPONENTS Widgets)
    if(Qt6_FOUND)
        set(FILL_QT_TARGET Qt6::Widgets)
    else()
        find_package(Qt5 5.15 QUIET COMPONENTS Widgets)
        if(NOT Qt5_FOUND)
            message(FATAL_ERROR
                "Qt preview requested, but no Qt 5.15+/Qt 6 Widgets kit was found")
        endif()
        set(FILL_QT_TARGET Qt5::Widgets)
    endif()
    add_executable(fill_preview tools/qt_preview.cpp)
    target_link_libraries(fill_preview PRIVATE ${FILL_QT_TARGET})
    if(MSVC)
        target_compile_options(fill_preview PRIVATE /utf-8)
    endif()
endif()

if(FILL_PIPELINE_BUILD_TESTS)
    enable_testing()
    add_executable(fill_tests tests/tests.cpp)
    target_link_libraries(fill_tests PRIVATE fill_core)
    add_test(NAME fill_tests COMMAND fill_tests)
endif()

install(TARGETS fill_pipeline RUNTIME DESTINATION bin)
