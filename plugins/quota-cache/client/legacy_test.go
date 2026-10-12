package client

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The drop-in first, then Quota Glance's default data-dir beside quota-cache's,
// then Quota Glance's default against the working directory: absolute,
// cleaned, and each path once.
func TestLegacySettingsCandidates(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	// t.TempDir can sit under a symlink; Abs uses the directory as Getwd
	// reports it.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defaultGlance := filepath.Join(wd, "plugins", "data", "quota-glance", "settings.json")
	for _, tc := range []struct {
		name, snapshot string
		want           []string
	}{
		{"the documented Docker layout", "/CLIProxyAPI/plugins/data/quota-cache/snapshot.json", []string{
			"/CLIProxyAPI/plugins/data/quota-cache/quota-glance-settings.json",
			"/CLIProxyAPI/plugins/data/quota-glance/settings.json",
			defaultGlance,
		}},
		{"the default path, where the sibling is the default", DefaultPath, []string{
			filepath.Join(wd, "plugins", "data", "quota-cache", "quota-glance-settings.json"),
			defaultGlance,
		}},
		{"a path to clean", "/srv/cpa/../cpa/data/./cache/snapshot.json", []string{
			"/srv/cpa/data/cache/quota-glance-settings.json",
			"/srv/cpa/data/quota-glance/settings.json",
			defaultGlance,
		}},
		{"a snapshot in the root", "/snapshot.json", []string{
			"/quota-glance-settings.json",
			"/quota-glance/settings.json",
			defaultGlance,
		}},
	} {
		if got := LegacySettingsCandidates(tc.snapshot); !slices.Equal(got, tc.want) {
			t.Errorf("%s:\n got %v\nwant %v", tc.name, got, tc.want)
		}
	}
	if LegacySettingsDropIn != "quota-glance-settings.json" {
		t.Errorf("LegacySettingsDropIn = %q", LegacySettingsDropIn)
	}
}

// Finding the candidates touches no file: none is created, and none needs to
// exist.
func TestLegacySettingsCandidatesTouchNothing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	snapshot := filepath.Join(dir, "plugins", "data", "quota-cache", "snapshot.json")
	for _, path := range LegacySettingsCandidates(snapshot) {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("the directory holds %v (%v)", entries, err)
	}
}
