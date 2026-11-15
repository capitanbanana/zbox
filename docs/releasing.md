# Releasing

`install.sh` installs a named GitHub release and pinned upstream dependencies.
It uses only macOS system commands, installs into a user-owned prefix, and
does not create VMs or change their state. The default release is deliberately
pinned in the script instead of resolving an untested moving `latest` tag.

Each release contains:

- `agents-vX.Y.Z-darwin-arm64.tar.gz`: Mac ARM executable and Linux ARM helper.
- `agents-vX.Y.Z-darwin-amd64.tar.gz`: Mac Intel executable and Linux x86 helper.
- `SHA256SUMS`: checksums of both archives.

Archives are built with `-trimpath -buildvcs=false`, without developer paths or
local Git metadata. Archive owners, timestamps, and extended attributes are
normalized rather than copied from the builder. `agents --version` reports the release injected at build
time. Lima and sing-box are downloaded separately from upstream; their full
archive layouts and included licenses are retained.

## Publish a release

1. Update the default version in `install.sh` and the pinned example in the
   README. If dependencies change, update both architecture checksums from
   upstream release metadata, then verify VM startup and guarded networking.
2. Run `make test` and `make release VERSION=vX.Y.Z`.
3. Inspect the changes and scan the commit history and release contents for
   secrets and personal paths. Commit the source, then create and push the
   version tag. Upload the archives to a draft GitHub release using the
   commands below.
4. Review the draft and publish it. Push the matching installer to `main` so
   the default curl command selects that release.
5. Run the public curl command with `--prefix` pointing to a temporary directory
   and `--no-modify-path`. Check `agents --version`, create and boot a temporary
   VM with a system-only host PATH, then delete the VM. Test actual Intel
   hardware before claiming native Intel VM validation.

Publish using the same build script:

```sh
make release VERSION=vX.Y.Z
gh release create vX.Y.Z dist/agents-vX.Y.Z-darwin-*.tar.gz dist/SHA256SUMS \
  --verify-tag --draft --title vX.Y.Z --notes-file "$RELEASE_NOTES"
# Review the draft before publishing:
gh release edit vX.Y.Z --draft=false
```

The release script and installer validate stable `vX.Y.Z` versions. Assets of
a published version should not be replaced; make a new version for a fix.

## Installer checks

`python3 scripts/test-install.py` runs offline integration tests with temporary
archives and mocked downloads. The tests cover checksums for both the CLI and
dependencies, preserving an existing command on failure, retry cleanup,
repeated installs, profile idempotence, paths with spaces and apostrophes,
argument forwarding, Intel/Rosetta selection, unsupported platforms, root
rejection, and concurrent-install exclusion. No test rewrites the user's
shell files or installs a VM.

These tests replace pinned hashes only in a temporary test copy of the script.
The distributed installer has no switch to bypass checksums. Actual binaries,
upstream downloads, and VM provisioning still need a macOS smoke test.

An interrupted install cleans up staging files and leaves the previous launcher
active. If the process is killed with SIGKILL, remove the stale
`<prefix>/share/agents/.install-lock` directory only after confirming that no
installer is still running, then rerun the command.
