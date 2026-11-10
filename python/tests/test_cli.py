"""Tests for the Python launcher that delegates to the npm lv24-brain CLI."""

from __future__ import annotations

import contextlib
import io
import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SRC_DIR = Path(__file__).resolve().parents[1] / "src"
if str(SRC_DIR) not in sys.path:
    sys.path.insert(0, str(SRC_DIR))

from lv24_brain import __version__
from lv24_brain.cli import NPM_PACKAGE, NPM_VERSION_ENV, main

_FAKE_NPX = '''\
import json
import os
import sys

record = {
    "argv": sys.argv[1:],
    "env": {
        key: value
        for key, value in os.environ.items()
        if key.startswith("BRAIN_") or key == "PATH"
    },
}
with open(os.environ["BRAIN_TEST_LOG"], "a", encoding="utf-8") as handle:
    handle.write(json.dumps(record, sort_keys=True) + "\\n")
raise SystemExit(3)
'''


class MainTests(unittest.TestCase):
    def setUp(self):
        self._tempdir = tempfile.TemporaryDirectory()
        self.addCleanup(self._tempdir.cleanup)
        self.root = Path(self._tempdir.name)
        self.bin_dir = self.root / "bin"
        self.bin_dir.mkdir()
        self.empty_dir = self.root / "empty"
        self.empty_dir.mkdir()
        self.log_path = self.root / "calls.jsonl"

        self._saved_env = {
            key: os.environ.get(key)
            for key in ("BRAIN_NPM_VERSION", "BRAIN_TEST_LOG")
        }
        self.addCleanup(self._restore_env)
        os.environ.pop("BRAIN_NPM_VERSION", None)
        os.environ["BRAIN_TEST_LOG"] = str(self.log_path)

        self._write_fake_npx()

    def _restore_env(self):
        for key, value in self._saved_env.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def _write_fake_npx(self):
        script = self.bin_dir / "fake_npx.py"
        script.write_text(_FAKE_NPX, encoding="utf-8")

        if os.name == "nt":
            launcher = self.bin_dir / "npx.cmd"
            content = '@echo off\r\n"{0}" "%~dp0fake_npx.py" %*\r\n'.format(
                sys.executable
            )
            with launcher.open("w", encoding="utf-8", newline="") as handle:
                handle.write(content)
        else:
            launcher = self.bin_dir / "npx"
            content = '#!/bin/sh\nexec "{0}" "${{0%/*}}/fake_npx.py" "$@"\n'.format(
                sys.executable
            )
            launcher.write_text(content, encoding="utf-8")
            launcher.chmod(
                launcher.stat().st_mode
                | stat.S_IXUSR
                | stat.S_IXGRP
                | stat.S_IXOTH
            )

    def _invoke(self, args, *, env=None, path=None, cwd=None):
        call_env = {
            "PATH": str(self.bin_dir if path is None else path),
            "BRAIN_TEST_LOG": str(self.log_path),
        }
        if os.name == "nt":
            call_env["PATHEXT"] = ".COM;.EXE;.BAT;.CMD"
        if env:
            call_env.update(env)

        stderr = io.StringIO()
        previous_cwd = os.getcwd()
        if cwd is not None:
            os.chdir(cwd)
        try:
            with mock.patch.dict(os.environ, call_env, clear=False):
                with contextlib.redirect_stderr(stderr):
                    code = main(list(args))
        finally:
            if cwd is not None:
                os.chdir(previous_cwd)
        return code, stderr.getvalue()

    def _calls(self):
        if not self.log_path.exists():
            return []
        return [
            json.loads(line)
            for line in self.log_path.read_text(encoding="utf-8").splitlines()
            if line
        ]

    def test_forwards_arguments_and_returns_child_exit_code(self):
        code, stderr = self._invoke(["open", ".", "--json"])

        self.assertEqual(code, 3)
        self.assertEqual(stderr, "")
        calls = self._calls()
        self.assertEqual(len(calls), 1)
        self.assertEqual(
            calls[0]["argv"],
            [
                "--yes",
                "{0}@{1}".format(NPM_PACKAGE, __version__),
                "open",
                ".",
                "--json",
            ],
        )

    def test_version_matches_expected_release(self):
        self.assertEqual(__version__, "0.12.1")

    def test_uses_env_override_and_inherits_it(self):
        code, stderr = self._invoke(
            ["--help"], env={NPM_VERSION_ENV: "latest"}
        )

        self.assertEqual(code, 3)
        self.assertEqual(stderr, "")
        call = self._calls()[0]
        self.assertEqual(
            call["argv"],
            ["--yes", "{0}@latest".format(NPM_PACKAGE), "--help"],
        )
        self.assertEqual(call["env"][NPM_VERSION_ENV], "latest")

    def test_blank_env_override_falls_back_to_package_version(self):
        code, _ = self._invoke([], env={NPM_VERSION_ENV: "  "})

        self.assertEqual(code, 3)
        self.assertEqual(
            self._calls()[0]["argv"],
            ["--yes", "{0}@{1}".format(NPM_PACKAGE, __version__)],
        )

    def test_missing_npx_returns_127_with_install_hint(self):
        code, stderr = self._invoke(
            [], path=self.empty_dir, cwd=self.empty_dir
        )

        self.assertEqual(code, 127)
        self.assertIn("npx", stderr)
        self.assertIn("Node.js", stderr)
        self.assertIn("npm", stderr)
        self.assertIn("installers", stderr)
        self.assertEqual(self._calls(), [])

    def test_keyboard_interrupt_returns_130(self):
        with mock.patch(
            "lv24_brain.cli.subprocess.run",
            side_effect=KeyboardInterrupt,
        ):
            code, stderr = self._invoke([])

        self.assertEqual(code, 130)
        self.assertEqual(stderr, "")

    def test_oserror_returns_127(self):
        with mock.patch(
            "lv24_brain.cli.subprocess.run",
            side_effect=OSError("not executable"),
        ):
            code, stderr = self._invoke([])

        self.assertEqual(code, 127)
        self.assertIn("failed to start", stderr)
        self.assertIn("not executable", stderr)


if __name__ == "__main__":
    unittest.main()
