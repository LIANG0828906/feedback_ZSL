cmake_minimum_required(VERSION 3.16)

project(MeshEditor
    VERSION 1.0.0
    DESCRIPTION "Deterministic OBJ/STL mesh editor with versioned conversion"
    LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

option(MESHEDITOR_BUILD_GUI "Build the Qt Widgets/OpenGL desktop application" ON)
option(MESHEDITOR_BUILD_TESTS "Build the headless regression tests" ON)

add_library(mesheditor_core STATIC
    src/core/Error.cpp
    src/core/Geometry.cpp
    src/core/Mesh.cpp
    src/core/Topology.cpp
    src/core/ObjIo.cpp
    src/core/StlIo.cpp
    src/core/FormatIo.cpp
    src/core/Welding.cpp
    src/core/VersionCompat.cpp
    src/core/Editing.cpp
    src/core/Deviation.cpp
    src/core/Project.cpp
    src/core/SampleGenerator.cpp
)
target_include_directories(mesheditor_core PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}/src")

if(MSVC)
    target_compile_options(mesheditor_core PRIVATE /W4 /permissive-)
else()
    target_compile_options(mesheditor_core PRIVATE -Wall -Wextra -Wpedantic)
endif()

add_executable(mesheditor_cli src/cli/main.cpp)
target_link_libraries(mesheditor_cli PRIVATE mesheditor_core)

if(MESHEDITOR_BUILD_TESTS)
    enable_testing()
    add_executable(mesheditor_tests tests/test_main.cpp)
    target_link_libraries(mesheditor_tests PRIVATE mesheditor_core)
    add_test(NAME mesheditor_core_tests COMMAND mesheditor_tests)
    add_test(NAME mesheditor_regression
             COMMAND mesheditor_cli regression
                     "${CMAKE_CURRENT_BINARY_DIR}/regression_workspace")
endif()

if(MESHEDITOR_BUILD_GUI)
    find_package(Qt6 QUIET COMPONENTS Widgets OpenGLWidgets)
    if(Qt6_FOUND)
        set(MESHEDITOR_QT_TARGET Qt6::Widgets)
        list(APPEND MESHEDITOR_QT_TARGET Qt6::OpenGLWidgets)
        set(MESHEDITOR_QT_LABEL "Qt6")
    else()
        find_package(Qt5 QUIET COMPONENTS Widgets OpenGL)
        if(Qt5_FOUND)
            set(MESHEDITOR_QT_TARGET Qt5::Widgets Qt5::OpenGL)
            set(MESHEDITOR_QT_LABEL "Qt5")
        endif()
    endif()

    if(MESHEDITOR_QT_TARGET)
        message(STATUS "Building Qt GUI with ${MESHEDITOR_QT_LABEL}")
        set(CMAKE_AUTOMOC ON)
        add_executable(mesheditor
            src/app/main.cpp
            src/app/MainWindow.cpp
            src/app/MainWindow.h
            src/app/MeshView.cpp
            src/app/MeshView.h
            src/app/BackgroundJob.cpp
            src/app/BackgroundJob.h
            src/app/Dialogs.cpp
            src/app/Dialogs.h
        )
        target_link_libraries(mesheditor PRIVATE mesheditor_core ${MESHEDITOR_QT_TARGET})
        target_compile_definitions(mesheditor PRIVATE
            MESHEDITOR_QT_MAJOR=$<IF:$<BOOL:${Qt6_FOUND}>,6,5>)
    else()
        message(WARNING "Qt5/Qt6 Widgets or OpenGL modules were not found; GUI target is disabled.")
    endif()
endif()

include(GNUInstallDirs)
install(TARGETS mesheditor_cli RUNTIME DESTINATION "${CMAKE_INSTALL_BINDIR}")
if(TARGET mesheditor)
    install(TARGETS mesheditor RUNTIME DESTINATION "${CMAKE_INSTALL_BINDIR}")
endif()
