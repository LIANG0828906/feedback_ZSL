cmake_minimum_required(VERSION 3.20)

project(ChunkPipeline VERSION 1.0.0 LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(CHUNKPIPE_BUILD_TESTS "Build deterministic unit and integration tests" ON)

find_package(Threads REQUIRED)

add_library(chunkpipe_core
    src/archive.cpp
    src/compare.cpp
    src/error.cpp
    src/generator.cpp
    src/hash.cpp
    src/model.cpp
    src/pipeline.cpp
    src/recovery.cpp
    src/trace.cpp
    src/visibility.cpp
)

target_include_directories(chunkpipe_core PUBLIC
    ${CMAKE_CURRENT_SOURCE_DIR}/include
)

target_link_libraries(chunkpipe_core PUBLIC Threads::Threads)

if(WIN32)
    target_compile_definitions(chunkpipe_core PUBLIC NOMINMAX WIN32_LEAN_AND_MEAN)
endif()

if(MSVC)
    target_compile_options(chunkpipe_core PRIVATE /W4 /permissive- /utf-8)
else()
    target_compile_options(chunkpipe_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(chunkpipe tools/chunkpipe.cpp)
target_link_libraries(chunkpipe PRIVATE chunkpipe_core)

include(CTest)
if(BUILD_TESTING AND CHUNKPIPE_BUILD_TESTS)
    add_executable(chunkpipe_tests tests/test_main.cpp)
    target_link_libraries(chunkpipe_tests PRIVATE chunkpipe_core)
    add_test(NAME chunkpipe_tests COMMAND chunkpipe_tests)
endif()

