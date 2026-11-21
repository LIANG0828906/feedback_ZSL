cmake_minimum_required(VERSION 3.24)

project(font_atlas_workbench VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(FONT_ATLAS_BUILD_GUI "Build the Qt 6 workbench when Qt is available" ON)
option(FONT_ATLAS_BUILD_TESTS "Build deterministic verification tests" ON)

add_library(font_atlas_core
    src/atlas.cpp
    src/engine.cpp
    src/font.cpp
    src/io.cpp
    src/json.cpp
    src/raster.cpp
    src/sha256.cpp
    src/snapshot.cpp
    src/trace.cpp
    src/types.cpp
    src/workbench.cpp
)

target_include_directories(font_atlas_core PUBLIC
    $<BUILD_INTERFACE:${CMAKE_CURRENT_SOURCE_DIR}/include>
    $<INSTALL_INTERFACE:include>
)

target_compile_features(font_atlas_core PUBLIC cxx_std_17)

if(MSVC)
    target_compile_options(font_atlas_core PRIVATE /W4 /permissive-)
else()
    target_compile_options(font_atlas_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

find_package(Threads REQUIRED)
target_link_libraries(font_atlas_core PUBLIC Threads::Threads)

add_executable(font_atlas app/main.cpp)
target_link_libraries(font_atlas PRIVATE font_atlas_core)

if(FONT_ATLAS_BUILD_TESTS)
    enable_testing()
    add_executable(font_atlas_tests tests/test_core.cpp)
    target_link_libraries(font_atlas_tests PRIVATE font_atlas_core)
    add_test(NAME font_atlas_core_tests COMMAND font_atlas_tests)
    set_tests_properties(font_atlas_core_tests PROPERTIES
        WORKING_DIRECTORY ${CMAKE_CURRENT_BINARY_DIR}
    )
    if(UNIX)
        add_test(NAME font_atlas_end_to_end
            COMMAND bash ${CMAKE_CURRENT_SOURCE_DIR}/scripts/verify_end_to_end.sh
                $<TARGET_FILE:font_atlas>
                ${CMAKE_CURRENT_BINARY_DIR}/e2e
        )
        set_tests_properties(font_atlas_end_to_end PROPERTIES
            WORKING_DIRECTORY ${CMAKE_CURRENT_BINARY_DIR}
            TIMEOUT 180
        )
    endif()
endif()

if(FONT_ATLAS_BUILD_GUI)
    find_package(Qt6 6.5 QUIET COMPONENTS Widgets OpenGLWidgets)
    if(Qt6_FOUND)
        add_executable(font_atlas_gui
            gui/main.cpp
            gui/MainWindow.cpp
            gui/MainWindow.h
            gui/PreviewWidget.cpp
            gui/PreviewWidget.h
        )
        target_link_libraries(font_atlas_gui PRIVATE
            font_atlas_core
            Qt6::Widgets
            Qt6::OpenGLWidgets
        )
        target_compile_features(font_atlas_gui PRIVATE cxx_std_17)
        set_target_properties(font_atlas_gui PROPERTIES AUTOMOC ON)
        message(STATUS "Qt 6.5+ found: building font_atlas_gui")
    else()
        message(STATUS "Qt 6.5+ Widgets/OpenGLWidgets not found: GUI target skipped")
    endif()
endif()

