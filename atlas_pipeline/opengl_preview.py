from __future__ import annotations

import ctypes
import os
import threading
from dataclasses import dataclass

import numpy as np

_GL_LOCK = threading.Lock()


@dataclass(frozen=True)
class OpenGLInfo:
    available: bool
    version: str
    renderer: str
    error: str | None = None

    def to_dict(self) -> dict[str, object]:
        return {
            "available": self.available,
            "version": self.version,
            "renderer": self.renderer,
            "error": self.error,
        }


def _egl_info() -> tuple[object, object, object, object, OpenGLInfo]:
    from OpenGL import EGL, GL
    from OpenGL.EGL import (
        EGLConfig,
        EGL_CONTEXT_CLIENT_VERSION,
        EGL_GREEN_SIZE,
        EGL_HEIGHT,
        EGL_NONE,
        EGL_NO_CONTEXT,
        EGL_NO_SURFACE,
        EGL_OPENGL_ES2_BIT,
        EGL_PBUFFER_BIT,
        EGL_RED_SIZE,
        EGL_RENDERABLE_TYPE,
        EGL_SURFACE_TYPE,
        EGL_WIDTH,
        eglChooseConfig,
        eglCreateContext,
        eglCreatePbufferSurface,
        eglGetDisplay,
        eglInitialize,
        eglMakeCurrent,
        eglQueryString,
    )

    display = eglGetDisplay(EGL.EGL_DEFAULT_DISPLAY)
    if not display:
        raise RuntimeError("eglGetDisplay failed")
    major = ctypes.c_int()
    minor = ctypes.c_int()
    if not eglInitialize(display, ctypes.byref(major), ctypes.byref(minor)):
        raise RuntimeError("eglInitialize failed")
    config_attrs = (
        EGL_SURFACE_TYPE,
        EGL_PBUFFER_BIT,
        EGL_RENDERABLE_TYPE,
        EGL_OPENGL_ES2_BIT,
        EGL_RED_SIZE,
        8,
        EGL_GREEN_SIZE,
        8,
        EGL_NONE,
    )
    config = EGLConfig()
    count = ctypes.c_int()
    if not eglChooseConfig(
        display,
        config_attrs,
        ctypes.byref(config),
        1,
        ctypes.byref(count),
    ) or count.value < 1:
        raise RuntimeError("no EGL OpenGL ES2 pbuffer config")
    context = eglCreateContext(
        display,
        config,
        EGL_NO_CONTEXT,
        (EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE),
    )
    if not context:
        raise RuntimeError("eglCreateContext failed")
    surface = eglCreatePbufferSurface(
        display,
        config,
        (EGL_WIDTH, 16, EGL_HEIGHT, 16, EGL_NONE),
    )
    if not surface:
        raise RuntimeError("eglCreatePbufferSurface failed")
    if not eglMakeCurrent(display, surface, surface, context):
        raise RuntimeError("eglMakeCurrent failed")
    version = GL.glGetString(GL.GL_VERSION)
    renderer = GL.glGetString(GL.GL_RENDERER)
    if isinstance(version, bytes):
        version = version.decode("ascii", errors="replace")
    if isinstance(renderer, bytes):
        renderer = renderer.decode("ascii", errors="replace")
    return display, context, surface, config, OpenGLInfo(True, str(version), str(renderer))


def _cleanup_egl(display: object, context: object, surface: object) -> None:
    from OpenGL.EGL import (
        EGL_NO_CONTEXT,
        EGL_NO_SURFACE,
        eglDestroyContext,
        eglDestroySurface,
        eglMakeCurrent,
        eglTerminate,
    )

    try:
        eglMakeCurrent(display, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT)
    except Exception:
        pass
    try:
        eglDestroySurface(display, surface)
    except Exception:
        pass
    try:
        eglDestroyContext(display, context)
    except Exception:
        pass
    try:
        eglTerminate(display)
    except Exception:
        pass


def check_opengl() -> OpenGLInfo:
    with _GL_LOCK:
        try:
            display, context, surface, _, info = _egl_info()
        except Exception as egl_exc:
            if os.name == "nt":
                try:
                    return _check_wgl()
                except Exception as wgl_exc:
                    return OpenGLInfo(
                        False,
                        "",
                        "",
                        f"EGL: {type(egl_exc).__name__}: {egl_exc}; "
                        f"WGL: {type(wgl_exc).__name__}: {wgl_exc}",
                    )
            return OpenGLInfo(False, "", "", f"{type(egl_exc).__name__}: {egl_exc}")
        _cleanup_egl(display, context, surface)
        return info


def _check_wgl() -> OpenGLInfo:
    """Create a hidden Win32 WGL context when EGL is unavailable."""

    import ctypes.wintypes as wt

    user32 = ctypes.WinDLL("user32", use_last_error=True)
    gdi32 = ctypes.WinDLL("gdi32", use_last_error=True)
    opengl32 = ctypes.WinDLL("opengl32", use_last_error=True)
    kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)

    class PIXELFORMATDESCRIPTOR(ctypes.Structure):
        _fields_ = [
            ("nSize", wt.WORD),
            ("nVersion", wt.WORD),
            ("dwFlags", wt.DWORD),
            ("iPixelType", wt.BYTE),
            ("cColorBits", wt.BYTE),
            ("cRedBits", wt.BYTE),
            ("cRedShift", wt.BYTE),
            ("cGreenBits", wt.BYTE),
            ("cGreenShift", wt.BYTE),
            ("cBlueBits", wt.BYTE),
            ("cBlueShift", wt.BYTE),
            ("cAlphaBits", wt.BYTE),
            ("cAlphaShift", wt.BYTE),
            ("cAccumBits", wt.BYTE),
            ("cAccumRedBits", wt.BYTE),
            ("cAccumGreenBits", wt.BYTE),
            ("cAccumBlueBits", wt.BYTE),
            ("cAccumAlphaBits", wt.BYTE),
            ("cDepthBits", wt.BYTE),
            ("cStencilBits", wt.BYTE),
            ("cAuxBuffers", wt.BYTE),
            ("iLayerType", wt.BYTE),
            ("bReserved", wt.BYTE),
            ("dwLayerMask", wt.DWORD),
            ("dwVisibleMask", wt.DWORD),
            ("dwDamageMask", wt.DWORD),
        ]

    class WNDCLASSW(ctypes.Structure):
        _fields_ = [
            ("style", wt.UINT),
            ("lpfnWndProc", ctypes.c_void_p),
            ("cbClsExtra", ctypes.c_int),
            ("cbWndExtra", ctypes.c_int),
            ("hInstance", wt.HINSTANCE),
            ("hIcon", wt.HICON),
            ("hCursor", wt.HANDLE),
            ("hbrBackground", wt.HBRUSH),
            ("lpszMenuName", wt.LPCWSTR),
            ("lpszClassName", wt.LPCWSTR),
        ]

    user32.CreateWindowExW.restype = wt.HWND
    user32.CreateWindowExW.argtypes = [
        wt.DWORD,
        wt.LPCWSTR,
        wt.LPCWSTR,
        wt.DWORD,
        ctypes.c_int,
        ctypes.c_int,
        ctypes.c_int,
        ctypes.c_int,
        wt.HWND,
        wt.HMENU,
        wt.HINSTANCE,
        ctypes.c_void_p,
    ]
    user32.GetDC.restype = wt.HDC
    user32.GetDC.argtypes = [wt.HWND]
    user32.ReleaseDC.restype = ctypes.c_int
    user32.ReleaseDC.argtypes = [wt.HWND, wt.HDC]
    user32.DestroyWindow.restype = wt.BOOL
    user32.DestroyWindow.argtypes = [wt.HWND]
    user32.RegisterClassW.restype = ctypes.c_ushort
    user32.RegisterClassW.argtypes = [ctypes.POINTER(WNDCLASSW)]
    user32.UnregisterClassW.restype = wt.BOOL
    user32.UnregisterClassW.argtypes = [wt.LPCWSTR, wt.HINSTANCE]
    kernel32.GetModuleHandleW.restype = wt.HINSTANCE
    kernel32.GetModuleHandleW.argtypes = [wt.LPCWSTR]
    gdi32.ChoosePixelFormat.restype = ctypes.c_int
    gdi32.ChoosePixelFormat.argtypes = [
        wt.HDC,
        ctypes.POINTER(PIXELFORMATDESCRIPTOR),
    ]
    gdi32.SetPixelFormat.restype = wt.BOOL
    gdi32.SetPixelFormat.argtypes = [
        wt.HDC,
        ctypes.c_int,
        ctypes.POINTER(PIXELFORMATDESCRIPTOR),
    ]
    opengl32.wglCreateContext.restype = ctypes.c_void_p
    opengl32.wglCreateContext.argtypes = [wt.HDC]
    opengl32.wglMakeCurrent.restype = wt.BOOL
    opengl32.wglMakeCurrent.argtypes = [wt.HDC, ctypes.c_void_p]
    opengl32.wglDeleteContext.restype = wt.BOOL
    opengl32.wglDeleteContext.argtypes = [ctypes.c_void_p]
    opengl32.glGetString.restype = ctypes.c_char_p
    opengl32.glGetString.argtypes = [ctypes.c_uint]

    hinstance = kernel32.GetModuleHandleW(None)
    class_name = f"AtlasPipelineGL_{os.getpid()}_{id(user32)}"
    wndclass = WNDCLASSW()
    wndclass.style = 0
    wndclass.lpfnWndProc = ctypes.cast(user32.DefWindowProcW, ctypes.c_void_p)
    wndclass.hInstance = hinstance
    wndclass.lpszClassName = class_name
    atom = user32.RegisterClassW(ctypes.byref(wndclass))
    if not atom and ctypes.get_last_error() != 1410:
        raise RuntimeError("RegisterClassW failed")
    hwnd = user32.CreateWindowExW(
        0,
        class_name,
        "AtlasPipelineHidden",
        0,
        0,
        0,
        16,
        16,
        None,
        None,
        hinstance,
        None,
    )
    if not hwnd:
        user32.UnregisterClassW(class_name, hinstance)
        raise RuntimeError("CreateWindowExW failed")
    hdc = user32.GetDC(hwnd)
    context = None
    try:
        pfd = PIXELFORMATDESCRIPTOR()
        pfd.nSize = ctypes.sizeof(PIXELFORMATDESCRIPTOR)
        pfd.nVersion = 1
        pfd.dwFlags = 4 | 32 | 1
        pfd.iPixelType = 0
        pfd.cColorBits = 32
        pixel_format = gdi32.ChoosePixelFormat(hdc, ctypes.byref(pfd))
        if not pixel_format:
            raise RuntimeError("ChoosePixelFormat failed")
        if not gdi32.SetPixelFormat(hdc, pixel_format, ctypes.byref(pfd)):
            raise RuntimeError("SetPixelFormat failed")
        context = opengl32.wglCreateContext(hdc)
        if not context:
            raise RuntimeError("wglCreateContext failed")
        if not opengl32.wglMakeCurrent(hdc, context):
            raise RuntimeError("wglMakeCurrent failed")
        version = opengl32.glGetString(0x1F02) or b""
        renderer = opengl32.glGetString(0x1F01) or b""
        return OpenGLInfo(
            True,
            version.decode("ascii", errors="replace"),
            renderer.decode("ascii", errors="replace"),
        )
    finally:
        if context:
            opengl32.wglMakeCurrent(hdc, None)
            opengl32.wglDeleteContext(context)
        user32.ReleaseDC(hwnd, hdc)
        user32.DestroyWindow(hwnd)
        user32.UnregisterClassW(class_name, hinstance)


_VERTEX_SHADER = b"""
#version 100
attribute vec2 position;
attribute vec2 texcoord;
varying vec2 uv;
void main() {
    uv = texcoord;
    gl_Position = vec4(position, 0.0, 1.0);
}
"""

_FRAGMENT_SHADER = b"""
#version 100
precision mediump float;
uniform sampler2D image;
varying vec2 uv;
void main() {
    gl_FragColor = texture2D(image, uv);
}
"""


def _compile_shader(source: bytes, kind: int) -> int:
    from OpenGL import GL

    shader = GL.glCreateShader(kind)
    GL.glShaderSource(shader, source)
    GL.glCompileShader(shader)
    if not GL.glGetShaderiv(shader, GL.GL_COMPILE_STATUS):
        log = GL.glGetShaderInfoLog(shader)
        raise RuntimeError(f"OpenGL shader compile failed: {log}")
    return int(shader)


def render_rgba_opengl(rgba: np.ndarray) -> np.ndarray:
    """Render an RGBA float image through a PyOpenGL EGL texture shader."""

    from OpenGL import GL
    from OpenGL.EGL import (
        EGL_NO_SURFACE,
        EGL_HEIGHT,
        EGL_NONE,
        EGL_WIDTH,
        eglCreatePbufferSurface,
        eglMakeCurrent,
    )

    array = np.asarray(rgba, dtype=np.float32)
    if array.ndim != 3 or array.shape[2] != 4:
        raise ValueError("OpenGL preview requires an RGBA image")
    height, width, _ = array.shape
    upload = np.floor(np.flipud(np.clip(array, 0.0, 1.0)) * 255.0 + 0.5).astype(
        np.uint8
    )
    with _GL_LOCK:
        display, context, initial_surface, config, _ = _egl_info()
        surface = initial_surface
        try:
            if width > 16 or height > 16:
                from OpenGL.EGL import eglDestroySurface

                eglMakeCurrent(display, EGL_NO_SURFACE, EGL_NO_SURFACE, context)
                eglDestroySurface(display, initial_surface)
                surface = eglCreatePbufferSurface(
                    display,
                    config,
                    (EGL_WIDTH, int(width), EGL_HEIGHT, int(height), EGL_NONE),
                )
                if not surface or not eglMakeCurrent(display, surface, surface, context):
                    raise RuntimeError("cannot create EGL preview pbuffer")
            vertex = _compile_shader(_VERTEX_SHADER, GL.GL_VERTEX_SHADER)
            fragment = _compile_shader(_FRAGMENT_SHADER, GL.GL_FRAGMENT_SHADER)
            program = GL.glCreateProgram()
            GL.glAttachShader(program, vertex)
            GL.glAttachShader(program, fragment)
            GL.glLinkProgram(program)
            if not GL.glGetProgramiv(program, GL.GL_LINK_STATUS):
                raise RuntimeError("OpenGL shader link failed")
            GL.glUseProgram(program)

            texture = GL.glGenTextures(1)
            GL.glBindTexture(GL.GL_TEXTURE_2D, texture)
            GL.glTexParameteri(GL.GL_TEXTURE_2D, GL.GL_TEXTURE_MIN_FILTER, GL.GL_NEAREST)
            GL.glTexParameteri(GL.GL_TEXTURE_2D, GL.GL_TEXTURE_MAG_FILTER, GL.GL_NEAREST)
            GL.glTexParameteri(GL.GL_TEXTURE_2D, GL.GL_TEXTURE_WRAP_S, GL.GL_CLAMP_TO_EDGE)
            GL.glTexParameteri(GL.GL_TEXTURE_2D, GL.GL_TEXTURE_WRAP_T, GL.GL_CLAMP_TO_EDGE)
            GL.glPixelStorei(GL.GL_UNPACK_ALIGNMENT, 1)
            GL.glTexImage2D(
                GL.GL_TEXTURE_2D,
                0,
                GL.GL_RGBA,
                width,
                height,
                0,
                GL.GL_RGBA,
                GL.GL_UNSIGNED_BYTE,
                upload,
            )

            positions = np.array(
                [-1.0, -1.0, 1.0, -1.0, -1.0, 1.0, 1.0, 1.0],
                dtype=np.float32,
            )
            texcoords = np.array(
                [0.0, 0.0, 1.0, 0.0, 0.0, 1.0, 1.0, 1.0],
                dtype=np.float32,
            )
            position_location = GL.glGetAttribLocation(program, "position")
            texcoord_location = GL.glGetAttribLocation(program, "texcoord")
            GL.glEnableVertexAttribArray(position_location)
            GL.glVertexAttribPointer(
                position_location,
                2,
                GL.GL_FLOAT,
                GL.GL_FALSE,
                0,
                positions,
            )
            GL.glEnableVertexAttribArray(texcoord_location)
            GL.glVertexAttribPointer(
                texcoord_location,
                2,
                GL.GL_FLOAT,
                GL.GL_FALSE,
                0,
                texcoords,
            )
            GL.glViewport(0, 0, width, height)
            GL.glClearColor(0.0, 0.0, 0.0, 0.0)
            GL.glClear(GL.GL_COLOR_BUFFER_BIT)
            GL.glDrawArrays(GL.GL_TRIANGLE_STRIP, 0, 4)
            data = GL.glReadPixels(
                0,
                0,
                width,
                height,
                GL.GL_RGBA,
                GL.GL_UNSIGNED_BYTE,
            )
            output = np.frombuffer(data, dtype=np.uint8).reshape(height, width, 4).copy()
            output = np.flipud(output)
            GL.glDeleteTextures([int(texture)])
            GL.glDeleteProgram(int(program))
            GL.glDeleteShader(int(vertex))
            GL.glDeleteShader(int(fragment))
            return output
        finally:
            _cleanup_egl(display, context, surface)
