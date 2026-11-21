cmake_minimum_required(VERSION 3.16)

project(meshrepair VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(MESHREPAIR_BUILD_UI "Build the optional OpenGL/GLFW snapshot viewer" ON)
option(MESHREPAIR_BUILD_TESTS "Build deterministic acceptance tests" ON)

find_package(Threads REQUIRED)

add_library(meshrepair
    src/core.cpp
    src/repair.cpp
    src/engine.cpp
    src/sample.cpp
)

target_include_directories(meshrepair
    PUBLIC
        ${CMAKE_CURRENT_SOURCE_DIR}/include
    PRIVATE
        ${CMAKE_CURRENT_SOURCE_DIR}/src
)

target_link_libraries(meshrepair PUBLIC Threads::Threads)

if(MSVC)
    target_compile_options(meshrepair PRIVATE /W4 /permissive-)
else()
    target_compile_options(meshrepair PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(meshrepair_cli src/main.cpp)
target_link_libraries(meshrepair_cli PRIVATE meshrepair)

if(MESHREPAIR_BUILD_UI)
    add_executable(meshrepair_ui src/ui.cpp)
    target_link_libraries(meshrepair_ui PRIVATE meshrepair)
    find_package(OpenGL QUIET)
    find_package(glfw3 3.2 QUIET)
    if(OpenGL_FOUND AND glfw3_FOUND)
        target_link_libraries(meshrepair_ui PRIVATE OpenGL::GL glfw)
        target_compile_definitions(meshrepair_ui PRIVATE MESHREPAIR_HAS_UI=1)
        message(STATUS "OpenGL/GLFW viewer enabled")
    else()
        message(STATUS "OpenGL/GLFW viewer uses a deterministic stub (headers or libraries not found)")
    endif()
endif()

if(MESHREPAIR_BUILD_TESTS)
    enable_testing()
    add_executable(meshrepair_tests tests/test_main.cpp)
    target_link_libraries(meshrepair_tests PRIVATE meshrepair)
    target_include_directories(meshrepair_tests PRIVATE ${CMAKE_CURRENT_SOURCE_DIR}/src)
    add_dependencies(meshrepair_tests meshrepair_cli)
    target_compile_definitions(meshrepair_tests PRIVATE
        MESHREPAIR_CLI_PATH="$<TARGET_FILE:meshrepair_cli>")
    add_test(NAME meshrepair_tests COMMAND meshrepair_tests)
    set_tests_properties(meshrepair_tests PROPERTIES TIMEOUT 120)
endif()

install(TARGETS meshrepair_cli RUNTIME DESTINATION bin)
install(DIRECTORY include/ DESTINATION include)
