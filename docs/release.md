# Release process

The release workflow (`.github/workflows/release.yml`) builds a five-target native matrix: Linux amd64/arm64 inside pinned `manylinux2014` containers on matching-architecture runners with a `readelf` GLIBC <= 2.17 gate, macOS amd64/arm64 and Windows amd64 natively. CGO `c-shared` libraries are never cross-compiled.

## Release matrix

| Asset | Library at ZIP root |
| --- | --- |
| `auto-baseline_<ver>_linux_amd64.zip` | `auto-baseline.so` |
| `auto-baseline_<ver>_linux_arm64.zip` | `auto-baseline.so` |
| `auto-baseline_<ver>_darwin_amd64.zip` | `auto-baseline.dylib` |
| `auto-baseline_<ver>_darwin_arm64.zip` | `auto-baseline.dylib` |
| `auto-baseline_<ver>_windows_amd64.zip` | `auto-baseline.dll` |
| `checksums.txt` | lowercase `sha256  bare-filename` lines |

Each ZIP contains exactly the library and `LICENSE` at the root (`internal/packaging.ValidateArchive`).

## 1. Establish the compatibility baseline

Confirm the target CPA revision. If it differs from `v7.2.146-3-g81e1b53`, re-audit every file:line in `docs/architecture.md`, the compiled fingerprint defaults in `internal/fingerprint/fingerprint.go`, the interceptor wire shape in `internal/hostapi/types.go`, and the Codex cloaking behaviour. Update `CompiledCPAVersion` and the docs.

## 2. Prepare the version

`PluginVersion` in `internal/plugin/runtime.go` is the single source of truth: the Makefile derives `VERSION` from it and `tools/packager` refuses a mismatching `-version`. Bump it, update `registry.json` if a `version` field is used, and add release notes.

## 3. Run local automated validation

```bash
export PATH=/path/to/go1.26.0/bin:$PATH
make fmt-check
make vet
make lint
make test
make race
make build
make package GLIBC_ENFORCE=0   # GLIBC_ENFORCE=1 (default) only on manylinux2014
make check-release
bash -n scripts/smoke-test.sh
```

## 4. Run the end-to-end smoke test

```bash
(cd /path/to/CLIProxyAPI && CGO_ENABLED=1 go build -o /tmp/cpa-bin/CLIProxyAPI ./cmd/server)
./scripts/smoke-test.sh /tmp/cpa-bin/CLIProxyAPI ./auto-baseline.so
```

Expected output ends with `smoke test PASSED`; evidence is under `dist/smoke/<run-id>/`.

## 5. Real-deployment dry-run acceptance

Install the candidate build in the real deployment with `dry-run: true`, drive real Claude Code / Codex traffic from at least two sessions, and confirm the pending candidate and dry-run history entry match the real client. Only then consider the release acceptable.

## 6. Review the release diff

Confirm no secrets, real management keys, or captured request bodies are in fixtures, docs, or smoke evidence. `dist/` is git-ignored.

## 7. Tag and push

```bash
VERSION=0.1.1
git tag -a "v${VERSION}" -m "auto-baseline v${VERSION}"
git push origin "v${VERSION}"
```

## 8. GitHub Actions release behavior

`release.yml` runs the test job on the tagged tree, builds all five targets, verifies the exact asset set with `tools/packager -verify -require ...`, writes `checksums.txt`, and creates or updates the GitHub release. Any failed or skipped job blocks publication.

## 9. Verify published assets

```bash
VERSION=0.1.1
mkdir -p "dist/release-v${VERSION}" && cd "dist/release-v${VERSION}"
gh release download "v${VERSION}" --repo NoorChasib/cpa-plugin-auto-baseline
cd ../..
go run ./tools/packager -verify -version "${VERSION}" -out "dist/release-v${VERSION}" \
  -require linux_amd64,linux_arm64,darwin_amd64,darwin_arm64,windows_amd64 \
  -checksums "dist/release-v${VERSION}/checksums.txt"
```

## 10. Verify custom-store update discovery

With the registry source configured, the Management Center's Plugin Store must list the new version. Install it and restart CPA.

## 11. Release notes checklist

- Audited CPA revision and compiled defaults assumed.
- Any change to classification rules, quorum defaults, or written keys.
- Any change to the state file schema (`statefile.SchemaVersion`); a bump discards old state with a warning.
- Reminder that in-place updates require a CPA restart.

## 12. Post-release operator acceptance

Follow the checklist in `README.md`.
