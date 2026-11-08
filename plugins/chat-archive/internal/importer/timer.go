package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Timer unit names. The deployment runbooks refer to chat-archive-import.timer.
const (
	systemdUnit  = "chat-archive-import"
	launchdLabel = "com.chat-archive.import"
)

// installTimer writes a systemd user timer (Linux) or a launchd agent (macOS) that runs `run` every 5
// minutes at low priority. It writes the files and prints the command that enables them.
// The systemd service also bounds memory, since the same host may run CPA or other heavy work beside
// it: Go's GC aims under GOMEMLIMIT, MemoryHigh throttles above 1.5 GiB and MemoryMax stops a runaway
// at 2 GiB. A run holds at most about one batch (maxHeldBytes) plus one whole file of up to 64 MiB
// being masked.
func (e *Env) installTimer() error {
	exe := e.Executable
	if exe == "" {
		p, err := os.Executable()
		if err != nil {
			return err
		}
		if p, err = filepath.EvalSymlinks(p); err != nil {
			return err
		}
		exe = p
	}
	switch e.goos() {
	case "linux":
		dir := filepath.Join(e.Home, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		service := fmt.Sprintf(`[Unit]
Description=chat-archive history importer (one sweep)

[Service]
Type=oneshot
ExecStart=%s run
Nice=10
IOSchedulingClass=idle
Environment=GOMEMLIMIT=1GiB
MemoryHigh=1536M
MemoryMax=2G
`, systemdQuote(exe))
		timer := `[Unit]
Description=chat-archive history importer every 5 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=5min
RandomizedDelaySec=30s

[Install]
WantedBy=timers.target
`
		if err := os.WriteFile(filepath.Join(dir, systemdUnit+".service"), []byte(service), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, systemdUnit+".timer"), []byte(timer), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "wrote %s/%s.{service,timer}\nenable: systemctl --user daemon-reload && systemctl --user enable --now %s.timer\n", dir, systemdUnit, systemdUnit)
	case "darwin":
		dir := filepath.Join(e.Home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>run</string></array>
  <key>StartInterval</key><integer>300</integer>
  <key>RunAtLoad</key><true/>
  <key>Nice</key><integer>10</integer>
  <key>LowPriorityIO</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>/dev/null</string>
</dict>
</plist>
`, launchdLabel, xmlEscape(exe))
		path := filepath.Join(dir, launchdLabel+".plist")
		if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "wrote %s\nenable: launchctl bootstrap gui/$(id -u) %s\n", path, path)
	default:
		return fmt.Errorf("install-timer supports linux and darwin, not %s", e.goos())
	}
	return nil
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
