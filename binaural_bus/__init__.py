"""Deterministic offline binaural bus synthesis."""

from .project import Project, load_project

__all__ = ["Project", "SynthesisResult", "load_project", "synthesize"]
__version__ = "0.1.0"


def __getattr__(name: str):
    if name in {"SynthesisResult", "synthesize"}:
        from .render import SynthesisResult, synthesize

        return {"SynthesisResult": SynthesisResult, "synthesize": synthesize}[name]
    raise AttributeError(name)

