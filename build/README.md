# Wails Build Assets

`appicon.png` is the only mandatory local desktop asset. The generated binaries
are written to `build/bin` by the Wails CLI.

Linux builds require GTK 3 and WebKitGTK development packages. Windows builds
use WebView2 and do not require a Node.js runtime when `wails build -s` is used.
