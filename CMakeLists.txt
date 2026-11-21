cmake_minimum_required(VERSION 3.20)

project(terrain_patch_viewer VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(TERRAIN_BUILD_TESTS "Build deterministic acceptance tests" ON)
option(TERRAIN_BUILD_DEMO "Build the terminal demo" ON)
option(TERRAIN_ENABLE_GLFW "Build the optional GLFW/OpenGL 3.3 preview window" OFF)
set(TERRAIN_GLFW_ROOT "" CACHE PATH "Local GLFW prefix containing include/ and lib/")
set(TERRAIN_GLAD_ROOT "" CACHE PATH "Local glad prefix containing include/glad and src/glad.c")

add_library(terrain_core
    src/status.cpp
    src/hash.cpp
    src/binary_io.cpp
    src/terrain_io.cpp
    src/sample_generator.cpp
    src/quadtree.cpp
    src/dependency.cpp
    src/surface.cpp
    src/mesh.cpp
    src/offscreen.cpp
    src/version_store.cpp
    src/scene.cpp
    src/async_controller.cpp
)

target_include_directories(terrain_core PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/include
)

if (MSVC)
    target_compile_options(terrain_core PRIVATE /W4 /permissive-)
else()
    target_compile_options(terrain_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(terrain_app src/main.cpp)
target_link_libraries(terrain_app PRIVATE terrain_core)

if (TERRAIN_BUILD_DEMO)
    add_executable(terrain_demo src/demo.cpp)
    target_link_libraries(terrain_demo PRIVATE terrain_core)
endif()

if (TERRAIN_ENABLE_GLFW)
    find_package(glfw3 QUIET)
    find_package(OpenGL QUIET)
    find_path(GLAD_INCLUDE_DIR glad/glad.h
        HINTS "${TERRAIN_GLAD_ROOT}/include" "${TERRAIN_GLAD_ROOT}")
    find_file(GLAD_SOURCE glad.c
        HINTS "${TERRAIN_GLAD_ROOT}/src" "${TERRAIN_GLAD_ROOT}")
    if (NOT glfw3_FOUND AND NOT TERRAIN_GLFW_ROOT STREQUAL "")
        find_path(GLFW_INCLUDE_DIR GLFW/glfw3.h
            HINTS "${TERRAIN_GLFW_ROOT}/include")
        find_library(GLFW_LIBRARY NAMES glfw3 glfw
            HINTS "${TERRAIN_GLFW_ROOT}/lib")
        if (GLFW_INCLUDE_DIR AND GLFW_LIBRARY)
            add_library(glfw INTERFACE)
            target_include_directories(glfw INTERFACE "${GLFW_INCLUDE_DIR}")
            target_link_libraries(glfw INTERFACE "${GLFW_LIBRARY}")
            set(glfw3_FOUND TRUE)
        endif()
    endif()
    if (glfw3_FOUND AND OpenGL_FOUND AND GLAD_INCLUDE_DIR AND GLAD_SOURCE)
        add_library(terrain_glad STATIC "${GLAD_SOURCE}")
        target_include_directories(terrain_glad PUBLIC "${GLAD_INCLUDE_DIR}")
        add_executable(terrain_gl_preview src/gl_preview.cpp)
        target_link_libraries(terrain_gl_preview PRIVATE
            terrain_core glfw terrain_glad OpenGL::GL)
        target_compile_definitions(terrain_gl_preview PRIVATE TERRAIN_HAVE_GLFW=1)
    else()
        message(WARNING "TERRAIN_ENABLE_GLFW=ON but local GLFW/OpenGL/glad were not found; GL preview skipped")
    endif()
endif()

if (TERRAIN_BUILD_TESTS)
    enable_testing()
    add_executable(terrain_tests tests/terrain_tests.cpp)
    target_link_libraries(terrain_tests PRIVATE terrain_core)
    find_package(Threads REQUIRED)
    target_link_libraries(terrain_tests PRIVATE Threads::Threads)
    target_link_libraries(terrain_app PRIVATE Threads::Threads)
    if (TARGET terrain_demo)
        target_link_libraries(terrain_demo PRIVATE Threads::Threads)
    endif()
    add_test(NAME terrain_acceptance COMMAND terrain_tests)
    set_tests_properties(terrain_acceptance PROPERTIES
        WORKING_DIRECTORY ${CMAKE_CURRENT_BINARY_DIR}
        TIMEOUT 120)
endif()

find_package(Threads REQUIRED)
target_link_libraries(terrain_core PUBLIC Threads::Threads)

