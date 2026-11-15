#!/usr/bin/env python3
"""Offline installer integration tests with real tar/hash/launcher operations."""
import hashlib
import io
import os
import re
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

REPO = Path(__file__).resolve().parent.parent
VERSION = re.search(r"local version=(v[0-9.]+)", (REPO / "install.sh").read_text()).group(1)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="agents-install-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.prefix = self.root / "user's install"
        self.mock = self.root / "mock"
        self.mock.mkdir()
        self.assets = self.root / "assets"
        self.assets.mkdir()
        self.shell = self.root / "shell"
        self.shell.mkdir()
        self.env = dict(os.environ, PATH=f"{self.mock}:/usr/bin:/bin:/usr/sbin:/sbin",
                        SHELL="/bin/zsh", ZDOTDIR=str(self.shell), MOCK_ASSETS=str(self.assets),
                        MOCK_LOG=str(self.root / "downloads"), MOCK_ARCH="arm64", MOCK_OS="Darwin")
        for name, content in {
            "uname": 'if [ "$1" = -s ]; then echo "$MOCK_OS"; else echo "$MOCK_ARCH"; fi',
            "id": 'echo "${MOCK_UID:-501}"',
            "sw_vers": 'echo "${MOCK_OS_VERSION:-13.5}"',
            "sysctl": 'echo "${MOCK_ROSETTA:-0}"',
        }.items():
            self.executable(self.mock / name, "#!/bin/sh\n" + content + "\n")
        self.executable(self.mock / "curl", f"#!{sys.executable}\n" + '''
import os, pathlib, shutil, sys
args = sys.argv[1:]
url = next(a for a in args if a.startswith("https://"))
with open(os.environ["MOCK_LOG"], "a") as f: f.write(url + "\\n")
source = pathlib.Path(os.environ["MOCK_ASSETS"]) / url.rsplit("/", 1)[1]
if not source.is_file() or os.environ.get("MOCK_DOWNLOAD_FAIL"): sys.exit(22)
shutil.copyfile(source, args[args.index("-o") + 1])
''')
        fake_agent = f'#!/bin/sh\nif [ "$1" = --version ]; then echo "agents {VERSION}"; else printf "%s\\n" "$PATH" "$@"; fi\n'
        app_sha = {}
        for arch in ("arm64", "amd64"):
            name = f"agents-{VERSION}-darwin-{arch}.tar.gz"
            app_sha[name] = self.archive(name, {"agents": fake_agent, f"agents-linux-{arch}": "guest"})
        (self.assets / "SHA256SUMS").write_text("".join(f"{digest}  {name}\n" for name, digest in app_sha.items()))
        lima = {"bin/limactl": '#!/bin/sh\necho "limactl version 2.1.1"\n'}
        sing = {"sing-box-1.13.11/sing-box": '#!/bin/sh\necho "sing-box version 1.13.11"\n'}
        hashes = [
            ("b6b0e6701189cd8c4e549cc39e6d054dc681487798b9b774ad2cbd30c08b2bd8", self.archive("lima-2.1.1-Darwin-arm64.tar.gz", lima)),
            ("2dc5b10aa3a4f26d08c1f3fe83e37e01f85a7d9db0d1d5cb6985b18af96ab07d", self.archive("lima-2.1.1-Darwin-x86_64.tar.gz", lima)),
            ("8fbeffbd6b737d0d3416428a126cce11002e60c89a006e42f1fbf6906802000b", self.archive("sing-box-1.13.11-darwin-arm64.tar.gz", sing)),
            ("09f7d509b10c499eac8d89470046d40a28f9f3e1b0fdea08202e6f2cc888f469", self.archive("sing-box-1.13.11-darwin-amd64.tar.gz", sing)),
        ]
        script = (REPO / "install.sh").read_text()
        # Only test fixtures replace pinned upstream hashes. The installer has
        # no environment switch to bypass checksums or redirect downloads.
        for original, fixture in hashes:
            self.assertIn(original, script)
            script = script.replace(original, fixture)
        self.script = self.root / "install.sh"
        self.script.write_text(script)

    def executable(self, path, data):
        path.write_text(data)
        path.chmod(0o755)

    def archive(self, name, files):
        target = self.assets / name
        with tarfile.open(target, "w:gz") as tar:
            for filename, content in files.items():
                raw = content.encode()
                info = tarfile.TarInfo(filename)
                info.mode = 0o755
                info.size = len(raw)
                tar.addfile(info, io.BytesIO(raw))
        return hashlib.sha256(target.read_bytes()).hexdigest()

    def install(self, *args, success=True):
        result = subprocess.run(["/bin/bash", str(self.script), "--prefix", str(self.prefix), *args],
                                env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_install_reuse_and_literal_paths(self):
        self.install()
        command = self.prefix / "bin/agents"
        output = subprocess.check_output([str(command), "argument with spaces", "$(false)"], env=self.env, text=True)
        self.assertIn("/lima/bin:", output)
        self.assertIn("/sing-box:", output)
        self.assertTrue(output.endswith("argument with spaces\n$(false)\n"))
        downloads = (self.root / "downloads").read_text()
        profile = (self.shell / ".zshrc").read_text()
        self.install()
        self.assertEqual((self.root / "downloads").read_text(), downloads)
        self.assertEqual((self.shell / ".zshrc").read_text(), profile)
        self.assertFalse((self.prefix / "share/agents/.install-lock").exists())

    def test_bad_checksum_preserves_existing_command(self):
        (self.prefix / "bin").mkdir(parents=True)
        command = self.prefix / "bin/agents"
        command.write_text("previous-install")
        (self.assets / f"agents-{VERSION}-darwin-arm64.tar.gz").write_bytes(b"corrupted")
        self.assertIn("SHA-256 mismatch", self.install(success=False).stderr)
        self.assertEqual(command.read_text(), "previous-install")
        self.assertEqual(list((self.prefix / "share/agents").iterdir()), [])

    def test_dependency_checksum_is_checked(self):
        (self.assets / "sing-box-1.13.11-darwin-arm64.tar.gz").write_bytes(b"corrupted")
        self.assertIn("SHA-256 mismatch", self.install(success=False).stderr)
        self.assertFalse((self.prefix / "bin/agents").exists())

    def test_download_failure_can_be_retried(self):
        self.env["MOCK_DOWNLOAD_FAIL"] = "1"
        self.install(success=False)
        self.assertEqual(list((self.prefix / "share/agents").iterdir()), [])
        del self.env["MOCK_DOWNLOAD_FAIL"]
        self.install("--no-modify-path")
        self.assertFalse((self.shell / ".zshrc").exists())

    def test_platform_and_root_checks_happen_before_install(self):
        for key, value in [("MOCK_OS", "Linux"), ("MOCK_UID", "0"), ("MOCK_OS_VERSION", "13.4"), ("MOCK_ARCH", "riscv64")]:
            old = self.env.get(key)
            self.env[key] = value
            self.install(success=False)
            self.assertFalse(self.prefix.exists())
            if old is None: del self.env[key]
            else: self.env[key] = old

    def test_intel_and_rosetta_selection(self):
        self.env["MOCK_ARCH"] = "x86_64"
        self.install("--no-modify-path")
        self.assertIn("darwin-amd64.tar.gz", (self.root / "downloads").read_text())
        self.env["MOCK_ROSETTA"] = "1"
        self.install("--no-modify-path")
        self.assertIn("darwin-arm64.tar.gz", (self.root / "downloads").read_text())

    def test_install_lock_prevents_concurrent_mutation(self):
        lock = self.prefix / "share/agents/.install-lock"
        lock.mkdir(parents=True)
        self.assertIn("Another install", self.install(success=False).stderr)
        self.assertTrue(lock.exists())
        self.assertFalse((self.root / "downloads").exists())


if __name__ == "__main__":
    unittest.main()
