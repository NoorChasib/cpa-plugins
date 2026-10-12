package client

import "path/filepath"

// LegacySettingsDropIn is the name of the file an operator may place beside
// the snapshot for quota-cache to import as Quota Glance's settings.json. It is
// the remedy for a Quota Glance data-dir that is not quota-cache's sibling:
// each plugin receives only its own configuration, so quota-cache cannot learn
// Quota Glance's.
const LegacySettingsDropIn = "quota-glance-settings.json"

// LegacySettingsCandidates is where quota-cache looks for Quota Glance's
// settings.json, for the snapshot at snapshotPath, in this order:
//
//  1. the drop-in, LegacySettingsDropIn in the snapshot's directory;
//  2. quota-glance/settings.json in the parent of the snapshot's directory,
//     which is Quota Glance's default data-dir beside quota-cache's
//     (/CLIProxyAPI/plugins/data/quota-glance/settings.json in the documented
//     Docker layout);
//  3. plugins/data/quota-glance/settings.json against the working directory,
//     Quota Glance's default as it resolves it in the same CPA process.
//
// Each is cleaned and made absolute, and a path already listed is left out, so
// the drop-in is always first. Quota Cache and Quota Glance both call it, so
// they agree on which file quota-cache would read. Nothing here touches the
// files: which of them exists, and which one is read, is the caller's rule.
func LegacySettingsCandidates(snapshotPath string) []string {
	dir := filepath.Dir(snapshotPath)
	candidates := []string{
		filepath.Join(dir, LegacySettingsDropIn),
		filepath.Join(filepath.Dir(dir), "quota-glance", "settings.json"),
		filepath.Join("plugins", "data", "quota-glance", "settings.json"),
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, path := range candidates {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		} else {
			path = filepath.Clean(path)
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}
