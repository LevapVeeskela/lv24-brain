"""Command-line entry point that delegates to the npm lv24-brain CLI."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from typing import Optional, Sequence

from . import __version__

NPM_PACKAGE = "lv24-brain"
NPM_VERSION_ENV = "BRAIN_NPM_VERSION"

_NPX_MISSING_MESSAGE = """\
lv24-brain: npx was not found on PATH.

This package is a thin Python launcher for the lv24-brain CLI distributed
through npm; it does not bundle Node.js or the CLI itself. Install Node.js 18
or newer (npm and npx are included) from https://nodejs.org/, or install the
native binary with the signed installers:
https://github.com/LevapVeeskela/lv24-brain/blob/main/docs/install.md
"""


def npm_spec() -> str:
    """Return the npm package spec, honoring BRAIN_NPM_VERSION."""
    version = os.environ.get(NPM_VERSION_ENV, "").strip() or __version__
    return "{0}@{1}".format(NPM_PACKAGE, version)


def main(argv: Optional[Sequence[str]] = None) -> int:
    """Run the npm CLI through npx and return its exit code."""
    args = list(sys.argv[1:] if argv is None else argv)

    npx = shutil.which("npx")
    if npx is None:
        sys.stderr.write(_NPX_MISSING_MESSAGE)
        return 127

    # No shell: the version and every user argument stay a single argv entry,
    # so nothing in them can be interpreted by a command interpreter.
    command = [npx, "--yes", npm_spec(), *args]

    try:
        completed = subprocess.run(command, check=False)
        return completed.returncode
    except KeyboardInterrupt:
        return 130
    except OSError as exc:
        sys.stderr.write(
            "lv24-brain: failed to start {0}: {1}\n".format(npx, exc)
        )
        return 127
