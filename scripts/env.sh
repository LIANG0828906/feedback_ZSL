# Source-able environment for the local (non-system) Qt6 / OpenCV 4 toolchain.
# No Docker, no system installs: everything lives under $HOME/.local.
export VF_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ -d "/home/liang/.local/qt6/usr" ]; then
  export VF_LOCAL_ROOT="/home/liang/.local"
else
  export VF_LOCAL_ROOT="$HOME/.local"
fi
export QT6_PREFIX="$VF_LOCAL_ROOT/qt6/usr"
export OPENCV_PREFIX="$VF_LOCAL_ROOT/opencv4/usr"
export VF_DEPS_PREFIX="$VF_LOCAL_ROOT/deps/usr"
export LD_LIBRARY_PATH="$QT6_PREFIX/lib/x86_64-linux-gnu:$OPENCV_PREFIX/lib/x86_64-linux-gnu:$OPENCV_PREFIX/lib/x86_64-linux-gnu/openblas-pthread:$VF_DEPS_PREFIX/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export QT_PLUGIN_PATH="$QT6_PREFIX/lib/x86_64-linux-gnu/qt6/plugins"
export QT_QPA_PLATFORM_PLUGIN_PATH="$QT6_PREFIX/lib/x86_64-linux-gnu/qt6/plugins/platforms"
