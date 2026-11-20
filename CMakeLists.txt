cmake_minimum_required(VERSION 3.16)
project(vforensics LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_AUTOMOC ON)
set(CMAKE_AUTORCC ON)

if(NOT CMAKE_BUILD_TYPE)
  set(CMAKE_BUILD_TYPE Release)
endif()

set(qt6_dir "$ENV{QT6_PREFIX}/lib/x86_64-linux-gnu/cmake/Qt6")
if(NOT EXISTS "${qt6_dir}/Qt6Config.cmake")
  set(qt6_dir "/home/liang/.local/qt6/usr/lib/x86_64-linux-gnu/cmake/Qt6")
endif()
set(Qt6_DIR "${qt6_dir}")
find_package(Qt6 REQUIRED COMPONENTS Core Gui Widgets Concurrent Test)

set(OpenCV_DIR "${CMAKE_CURRENT_SOURCE_DIR}/cmake")
set(OpenCVLocal_DIR "${CMAKE_CURRENT_SOURCE_DIR}/cmake")
find_package(OpenCVLocal 4 REQUIRED)
include_directories(${OpenCV_INCLUDE_DIRS})

add_compile_options(-Wall -Wextra -Wpedantic)

set(VF_CORE_SOURCES
  include/vforensics/forensic_worker.h
  src/types.cpp
  src/frame_sequence.cpp
  src/frame_sequence_writer.cpp
  src/manifest.cpp
  src/dependency_graph.cpp
  src/forensic_engine.cpp
  src/cache_store.cpp
  src/snapshot.cpp
  src/forensic_worker.cpp
  src/report.cpp
)

add_library(vforensics STATIC ${VF_CORE_SOURCES})
target_include_directories(vforensics PUBLIC include)
if(DEFINED ENV{VF_DEPS_PREFIX})
  set(vf_deps_lib "$ENV{VF_DEPS_PREFIX}/lib")
else()
  set(vf_deps_lib "/home/liang/.local/deps/usr/lib")
endif()
target_link_directories(vforensics PUBLIC "${vf_deps_lib}")
target_link_libraries(vforensics PUBLIC
  Qt6::Core Qt6::Concurrent
  ${OpenCV_LIBS}
  tbb z glib-2.0 pcre2-8 GLdispatch GLX X11 xcb Xau zstd
  openblas gfortran gomp Xdmcp atomic dbus-1 fontconfig
  harfbuzz png16 freetype xkbcommon xkbcommon-x11
  xcb-cursor xcb-image xcb-keysyms xcb-render-util xcb-icccm
  xcb-randr xcb-xkb xcb-xinput xcb-util
  systemd expat bz2 brotlidec brotlicommon graphite2 EGL
)

add_executable(vf_cli tools/vf_cli.cpp)
target_link_libraries(vf_cli PRIVATE vforensics)

add_executable(vf_generate tools/vf_generate.cpp)
target_link_libraries(vf_generate PRIVATE vforensics)

add_executable(vf_gui src/main_window.cpp src/main_gui.cpp src/timeline_widget.cpp)
target_link_libraries(vf_gui PRIVATE vforensics Qt6::Gui Qt6::Widgets)

enable_testing()
add_subdirectory(tests)
