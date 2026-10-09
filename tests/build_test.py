# Release-builder contract tests; Go compilation is stubbed, zip/SHA256 are real.
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
import zipfile

ROOT = Path(__file__).resolve().parents[1]
GIT_BASH = Path(r"C:\Program Files\Git\bin\bash.exe")
BASH = str(GIT_BASH) if GIT_BASH.is_file() else shutil.which("bash")


@unittest.skipUnless(BASH, "Bash is required to test build.sh")
class ReleaseBuilderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="artex-build-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        shutil.copyfile(ROOT / "build.sh", self.root / "build.sh")
        for name in ("config.example.json", "README.md", "start.sh", "start.bat"):
            (self.root / name).write_text("fixture", encoding="utf-8")
        (self.root / "skills").mkdir()
        (self.root / "skills" / "SKILL.md").write_text("fixture", encoding="utf-8")
        (self.root / "server" / "webui" / "dist").mkdir(parents=True)
        (self.root / "dist").mkdir()
        self.tools = self.root / "tools"
        self.tools.mkdir()
        go = self.tools / "go"
        go.write_text('''#!/bin/bash
set -eu
[ "$1" = build ] || exit 98
printf '%s/%s\\n' "$GOOS" "$GOARCH" >> "$ARTEX_TEST_BUILD_LOG"
shift
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output="$2"; shift; fi
  shift
done
printf 'binary %s/%s\\n' "$GOOS" "$GOARCH" > "$output"
''', encoding="utf-8")
        go.chmod(0o755)
        helper = self.tools / "zip_helper.py"
        helper.write_text('''import pathlib, sys, zipfile
archive, directory = sys.argv[-2:]
with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as z:
    for path in sorted(pathlib.Path(directory).rglob("*")):
        if path.is_file():
            z.write(path)
''', encoding="utf-8")
        zip_tool = self.tools / "zip"
        zip_tool.write_text('#!/bin/bash\npython "$ARTEX_TEST_ZIP_HELPER" "$@"\n', encoding="utf-8")
        zip_tool.chmod(0o755)
        self.env = {k: v for k, v in os.environ.items() if not k.upper().startswith("ARTEX_")}
        path = self.env.pop("PATH", self.env.pop("Path", ""))
        self.env["PATH"] = str(self.tools) + os.pathsep + path
        self.env.update(
            ARTEX_SKIP_FRONTEND="1", ARTEX_SKIP_NPM_CI="1",
            ARTEX_BUILD_VERSION="v9.8.7", ARTEX_COMPRESS="0", ARTEX_PACKAGE="1",
            ARTEX_TEST_BUILD_LOG=str(self.root / "build.log"),
            ARTEX_TEST_ZIP_HELPER=str(helper),
        )

    def build(self, *args, **env):
        return subprocess.run(
            [BASH, "-c", 'export PATH="$PWD/tools:$PATH"; exec bash ./build.sh "$@"', "build.sh", *args], cwd=self.root,
            env={**self.env, **env}, encoding="utf-8", errors="replace",
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60,
        )

    def checksums(self):
        return {
            name.removeprefix("*"): digest
            for digest, name in (
                line.split() for line in (self.root / "dist" / "SHA256SUMS").read_text().splitlines()
            )
        }

    def test_release_contains_all_five_platforms_and_valid_sha256(self):
        result = self.build("--release")
        self.assertEqual(result.returncode, 0, result.stdout)
        targets = ("linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64")
        sums = self.checksums()
        self.assertEqual(set(sums), {f"artex-9.8.7-{target}.zip" for target in targets})
        for target in targets:
            name = f"artex-9.8.7-{target}"
            archive = self.root / "dist" / f"{name}.zip"
            self.assertEqual(sums[archive.name], hashlib.sha256(archive.read_bytes()).hexdigest())
            with zipfile.ZipFile(archive) as z:
                files = set(z.namelist())
                binary, launcher = ("artex.exe", "start.bat") if target.startswith("windows") else ("artex", "start.sh")
                self.assertEqual(files, {
                    f"{name}/{binary}", f"{name}/{launcher}", f"{name}/config.example.json",
                    f"{name}/README.md", f"{name}/skills/SKILL.md",
                })
                if launcher == "start.sh" and os.name != "nt":
                    self.assertTrue((z.getinfo(f"{name}/{launcher}").external_attr >> 16) & 0o111)

    def test_checksums_exclude_stale_archives(self):
        (self.root / "dist" / "old-release.zip").write_bytes(b"stale archive")
        result = self.build("--target", "linux/amd64")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(set(self.checksums()), {"artex-9.8.7-linux-amd64.zip"})

    def test_unsupported_and_duplicate_targets_fail_before_compilation(self):
        for args, env in (
            (("--target", "windows/arm64"), {}),
            (("--release",), {"ARTEX_TARGETS": "linux/amd64,linux/amd64"}),
            (("--release",), {"ARTEX_TARGETS": " , "}),
        ):
            with self.subTest(args=args, env=env):
                result = self.build(*args, **env)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertFalse((self.root / "build.log").exists(), result.stdout)

    def test_checksum_failure_is_fatal(self):
        checksum = self.tools / "sha256sum"
        checksum.write_text("#!/bin/bash\nexit 1\n", encoding="utf-8")
        checksum.chmod(0o755)
        result = self.build("--target", "linux/amd64")
        self.assertNotEqual(result.returncode, 0, result.stdout)


if __name__ == "__main__":
    unittest.main()
