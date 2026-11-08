#!/usr/bin/env python3
"""Exercise both public registry contracts for chat-archive with the actual pinned CPA v8 SDK, offline.

Use --cpa-source with the verified, unmodified source directory printed by native
smoke. The temporary Go harness lives outside this project and outside CPA source.
The public URLs are mapped to local files; success does not mean they are hosted.
"""
import argparse
import importlib.util
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("release", ROOT / "scripts/release.py")
release = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(release)

HARNESS = r'''
package main
import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strings"
    store "github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginstore"
)
const sourceURL = "https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json"
const releaseBase = "https://github.com/NoorChasib/cpa-plugins/releases/download/chat-archive/v0.1.0"
const apiBase = "https://api.github.com/repos/NoorChasib/cpa-plugins/releases/"
const archiveName = "chat-archive_0.1.0_linux_amd64.zip"
type local struct { root, dist string; corrupt bool }
func (l local) Do(r *http.Request) (*http.Response, error) {
    // No real transport exists. Only the canonical metadata/files below are served.
    var raw []byte
    var err error
    name := ""
    switch r.URL.String() {
    case sourceURL: name = filepath.Join(l.root, "registry.json")
    case releaseBase + "/registry.json": name = filepath.Join(l.dist, "registry.json")
    case releaseBase + "/" + archiveName: name = filepath.Join(l.dist, archiveName)
    case releaseBase + "/checksums.txt":
        name = filepath.Join(l.dist, "checksums.txt")
        if l.corrupt { raw = []byte(strings.Repeat("0", 64) + "  " + archiveName + "\n") }
    case apiBase + "latest", apiBase + "tags/chat-archive/v0.1.0":
        fixture := store.Release{TagName: "chat-archive/v0.1.0"}
        for _, asset := range []string{archiveName, "checksums.txt", "registry.json"} {
            fixture.Assets = append(fixture.Assets, store.ReleaseAsset{Name: asset, BrowserDownloadURL: releaseBase + "/" + asset})
        }
        raw, err = json.Marshal(fixture)
    default: return nil, fmt.Errorf("unexpected URL: %s", r.URL.String())
    }
    if raw == nil && err == nil { raw, err = os.ReadFile(name) }
    if err != nil { return nil, err }
    return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
}
func main() {
    ctx := context.Background()
    for index, registryURL := range []string{sourceURL, releaseBase + "/registry.json"} {
        client := store.NewClient(local{os.Args[1], os.Args[2], false}, registryURL)
        registry, err := client.FetchRegistry(ctx); if err != nil { panic(err) }
        if registry.SchemaVersion != 2 || len(registry.Plugins) != 1 { panic("wrong registry") }
        plugin := registry.Plugins[0]
        if plugin.ID != "chat-archive" || plugin.Version != "0.1.0" { panic("wrong identity") }
        expectedType := store.InstallTypeGitHubRelease
        if index == 1 { expectedType = store.InstallTypeDirect }
        if store.PluginInstallType(plugin) != expectedType { panic("wrong install type") }
        if index == 1 {
            platforms := store.PluginPlatforms(plugin)
            if len(platforms) != 1 || platforms[0].GOOS != "linux" || platforms[0].GOARCH != "amd64" { panic("wrong platforms") }
        }
        options := store.InstallOptions{PluginsDir: filepath.Join(os.Args[3], expectedType), GOOS: "linux", GOARCH: "amd64"}
        if index == 0 {
            // CPA's github-release discovery accepts only plain vX.Y.Z release tags. This repository
            // tags per plugin (chat-archive/vX.Y.Z), so the published catalog pins direct artifacts;
            // the source registry's github-release entry must be refused, never resolved elsewhere.
            if _, err := client.Install(ctx, plugin, options); err == nil { panic("github-release install accepted a monorepo tag") }
            fmt.Println("PASS: actual CPA schema 2 github-release registry parses; its per-plugin release tag is refused, so only the direct catalog installs; no network")
            continue
        }
        installed, err := client.Install(ctx, plugin, options); if err != nil { panic(err) }
        if installed.Skipped || installed.Version != "0.1.0" || installed.InstallType != expectedType { panic("wrong initial install") }
        got, err := os.ReadFile(installed.Path); if err != nil { panic(err) }
        want, err := os.ReadFile(filepath.Join(os.Args[2], "chat-archive.so")); if err != nil { panic(err) }
        if !bytes.Equal(got, want) { panic("installed library changed") }
        again, err := client.Install(ctx, plugin, options); if err != nil { panic(err) }
        if !again.Skipped { panic("same-library install should be idempotent") }
        unsupported := options; unsupported.GOARCH = "arm64"
        if _, err = client.Install(ctx, plugin, unsupported); err == nil { panic("unsupported platform accepted") }
        if index == 0 {
            fixed, err := client.InstallVersion(ctx, plugin, "chat-archive/v0.1.0", "0.1.0", options)
            if err != nil || !fixed.Skipped { panic("exact-tag installation failed") }
            if _, err = client.InstallVersion(ctx, plugin, "chat-archive/v0.1.0", "0.2.0", options); err == nil { panic("wrong release version accepted") }
            bad := store.NewClient(local{os.Args[1], os.Args[2], true}, registryURL)
            if _, err = bad.Install(ctx, plugin, options); err == nil { panic("bad release checksum accepted") }
        } else {
            plugin.Install.Artifacts[0].SHA256 = strings.Repeat("0", 64)
            if _, err = client.Install(ctx, plugin, options); err == nil { panic("bad direct checksum accepted") }
        }
        fmt.Printf("PASS: actual CPA schema 2 %s registry, linux/amd64 checksum/ZIP install, byte equality, idempotence, unsupported platform and corrupt checksum rejection; no network\n", expectedType)
    }
}
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cpa-source", type=Path, required=True)
    parser.add_argument("--dist", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    release.verify(args.dist, release.candidate_version())
    with tempfile.TemporaryDirectory(prefix="chat-archive-validator-") as directory:
        path = Path(directory)
        (path / "main.go").write_text(HARNESS)
        subprocess.run(["go", "-C", str(args.cpa_source.resolve()), "run", str(path / "main.go"),
                        str(ROOT), str(args.dist.resolve()), str(path / "plugins")], check=True)


if __name__ == "__main__":
    main()
