# Minimal imported-target shim for the locally extracted OpenCV 4.10 packages
# (core + imgproc only). The upstream OpenCVModules.cmake references modules
# that were not installed, so the project declares exactly the two targets it
# uses instead of loading the full upstream module set.
set(_ocv_root "$ENV{OPENCV_PREFIX}")
if(NOT _ocv_root)
  if(DEFINED ENV{OPENCV_PREFIX})
    set(_ocv_root "$ENV{OPENCV_PREFIX}")
  else()
    set(_ocv_root "/home/liang/.local/opencv4/usr")
  endif()
endif()
set(OpenCV_VERSION 4.10.0)
set(OpenCV_INCLUDE_DIRS "${_ocv_root}/include/opencv4")
set(_ocv_libdir "${_ocv_root}/lib/x86_64-linux-gnu")
foreach(_m core imgproc)
  add_library(opencv_${_m} SHARED IMPORTED)
  set_target_properties(opencv_${_m} PROPERTIES
    INTERFACE_INCLUDE_DIRECTORIES "${OpenCV_INCLUDE_DIRS}"
    IMPORTED_LOCATION "${_ocv_libdir}/libopencv_${_m}.so.4.10.0")
endforeach()
set_target_properties(opencv_imgproc PROPERTIES INTERFACE_LINK_LIBRARIES opencv_core)
set(OpenCV_LIBS opencv_core opencv_imgproc)
set(OpenCV_FOUND TRUE)
