
"""Shared test fixtures: one deterministic corpus + index for the suite."""

import os
import shutil
import tempfile

from afpipe.config import Config
from afpipe.builder import IndexBuildManager
from afpipe.manifest import load_manifest
from afpipe.samples import DEFAULT_SEED, generate

WORKSPACE = tempfile.mkdtemp(prefix="afpipe-tests-")
CORPUS_DIR = os.path.join(WORKSPACE, "corpus")
WORK_DIR = os.path.join(WORKSPACE, "work")
INDEX_DIR = os.path.join(WORKSPACE, "index")

generate(CORPUS_DIR, seed=DEFAULT_SEED)
MANIFEST_PATH = os.path.join(CORPUS_DIR, "manifest.json")
MANIFEST = load_manifest(MANIFEST_PATH)
CONFIG = Config(max_workers=2).validate()
IndexBuildManager(WORK_DIR, INDEX_DIR, CONFIG, MANIFEST).build()


def fresh_dir(name: str) -> str:
    path = os.path.join(WORKSPACE, name)
    shutil.rmtree(path, ignore_errors=True)
    return path


def cleanup():
    shutil.rmtree(WORKSPACE, ignore_errors=True)
