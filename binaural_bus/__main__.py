"""Allow ``python -m binaural_bus`` to run the CLI."""

from .cli import main

raise SystemExit(main())
