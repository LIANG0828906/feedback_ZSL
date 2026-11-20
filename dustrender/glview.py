"""Minimal standalone PyOpenGL image window."""

from __future__ import annotations

import sys
from pathlib import Path

import numpy as np


def view(path: str | Path) -> None:
    from OpenGL import GL, GLUT

    image = np.load(path, allow_pickle=False).astype(np.float32)
    image = np.clip(np.flipud(image), 0.0, 1.0)
    height, width = image.shape[:2]
    window_width, window_height = 960, 720

    def display() -> None:
        GL.glClearColor(0.03, 0.035, 0.05, 1.0)
        GL.glClear(GL.GL_COLOR_BUFFER_BIT)
        GL.glMatrixMode(GL.GL_PROJECTION)
        GL.glLoadIdentity()
        GL.glOrtho(0, width, 0, height, -1, 1)
        GL.glMatrixMode(GL.GL_MODELVIEW)
        GL.glLoadIdentity()
        viewport = GL.glGetIntegerv(GL.GL_VIEWPORT)
        GL.glPixelZoom(float(viewport[2]) / width, float(viewport[3]) / height)
        GL.glRasterPos2f(0, 0)
        GL.glDrawPixels(width, height, GL.GL_RGB, GL.GL_FLOAT, image)
        GLUT.glutSwapBuffers()

    GLUT.glutInit([sys.argv[0]])
    GLUT.glutInitDisplayMode(GLUT.GLUT_RGB | GLUT.GLUT_DOUBLE)
    GLUT.glutInitWindowSize(window_width, window_height)
    GLUT.glutCreateWindow(b"DustRender OpenGL Preview")
    GLUT.glutDisplayFunc(display)
    GLUT.glutKeyboardFunc(lambda key, x, y: exit(0) if key in (b"\x1b", b"q") else None)
    GLUT.glutMainLoop()


if __name__ == "__main__":
    view(sys.argv[1])
