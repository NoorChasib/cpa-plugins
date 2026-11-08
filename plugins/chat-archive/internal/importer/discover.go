package importer

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// File roles of JSONL files (wire.ImportRecord.FileRole) and kinds of whole files
// (wire.ImportFile.Kind).
const (
	RoleMain            = "main"
	RoleSubagent        = "subagent"
	RoleWorkflowAgent   = "workflow_agent"
	RoleWorkflowJournal = "workflow_journal"
	RoleCodexRollout    = "codex_rollout"

	FileToolResult     = "tool_result_file"
	FileWorkflowRun    = "workflow_run"
	FileWorkflowScript = "workflow_script"
	FileAgentMeta      = "agent_meta"
	FileSkill          = "skill"
)

// Clients (wire.ImportRecord.Client).
const (
	ClientClaudeCode = "claude-code"
	ClientCodex      = "codex"
)

// maxSkillBytes: skills are imported only as text files of at most 1 MiB.
const maxSkillBytes = 1 << 20

// fileRef is one discovered source file.
type fileRef struct {
	Root      string
	Rel       string // slash-separated, relative to the root
	Path      string
	Key       string // "<root>/<rel>" (Locator.FileKey)
	Role      string // a Role* (JSONL records) or a File* kind (import.file)
	Client    string
	SessionID string // Claude: from the path (main file name or the <uuid>/ directory)
	AgentID   string // Claude agent files: from agent-<id>.jsonl
	RunID     string // workflow journals: the wf_<id> directory
	Whole     bool   // an import.file (whole file), not JSONL records
	Size      int64
	MtimeNS   int64
	Inode     uint64
}

// classify returns the role of a file under root, or ok=false for files the importer ignores.
func classify(root, rel string) (fileRef, bool) {
	f := fileRef{Root: root, Rel: rel, Key: root + "/" + rel}
	parts := strings.Split(rel, "/")
	name := parts[len(parts)-1]
	switch root {
	case RootCodexSessions:
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			f.Role, f.Client = RoleCodexRollout, ClientCodex
			return f, true
		}
		return f, false
	case RootClaudeSkills:
		f.Role, f.Client, f.Whole = FileSkill, ClientClaudeCode, true
		return f, true
	case RootClaudeProjects:
	default:
		return f, false
	}
	f.Client = ClientClaudeCode
	n := len(parts)
	if n == 2 && strings.HasSuffix(name, ".jsonl") {
		f.Role, f.SessionID = RoleMain, strings.TrimSuffix(name, ".jsonl")
		return f, true
	}
	if n < 4 {
		return f, false
	}
	f.SessionID = parts[1]
	agentFile := strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl")
	metaFile := strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".meta.json")
	switch {
	case n == 4 && parts[2] == "subagents" && agentFile:
		f.Role, f.AgentID = RoleSubagent, strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
	case n == 4 && parts[2] == "subagents" && metaFile:
		f.Role, f.Whole = FileAgentMeta, true
	case n == 6 && parts[2] == "subagents" && parts[3] == "workflows" && strings.HasPrefix(parts[4], "wf_"):
		switch {
		case agentFile:
			f.Role, f.AgentID, f.RunID = RoleWorkflowAgent, strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl"), parts[4]
		case metaFile:
			f.Role, f.Whole, f.RunID = FileAgentMeta, true, parts[4]
		case name == "journal.jsonl":
			f.Role, f.RunID = RoleWorkflowJournal, parts[4]
		default:
			return f, false
		}
	case n == 4 && parts[2] == "workflows" && strings.HasPrefix(name, "wf_") && strings.HasSuffix(name, ".json"):
		f.Role, f.Whole, f.RunID = FileWorkflowRun, true, strings.TrimSuffix(name, ".json")
	case n >= 5 && parts[2] == "workflows" && parts[3] == "scripts" && strings.HasSuffix(name, ".js"):
		f.Role, f.Whole = FileWorkflowScript, true
	case n == 4 && parts[2] == "tool-results":
		f.Role, f.Whole = FileToolResult, true
	default:
		return f, false
	}
	return f, true
}

// discover walks the roots and returns every importable regular file with its stat, sorted by key.
// Missing roots are skipped. A root that is a symlink (a projects directory on another disk) is
// resolved first, since WalkDir does not descend into a symlinked root; keys stay relative to the root.
func discover(roots map[string]string) ([]fileRef, error) {
	var out []fileRef
	names := make([]string, 0, len(roots))
	for r := range roots {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, root := range names {
		dir := roots[root]
		if dir == "" {
			continue
		}
		dir, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == dir {
					return err
				}
				return nil // unreadable entries are skipped
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return nil
			}
			f, ok := classify(root, filepath.ToSlash(rel))
			if !ok {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if f.Role == FileSkill && info.Size() > maxSkillBytes {
				return nil
			}
			f.Path = path
			f.Size, f.MtimeNS, f.Inode = info.Size(), info.ModTime().UnixNano(), inodeOf(info)
			out = append(out, f)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// statRef refreshes f's size, mtime and inode.
func statRef(f *fileRef) error {
	info, err := os.Stat(f.Path)
	if err != nil {
		return err
	}
	f.Size, f.MtimeNS, f.Inode = info.Size(), info.ModTime().UnixNano(), inodeOf(info)
	return nil
}

// matchesSession reports whether f belongs to the Claude Code session (or Codex thread) id: the main
// file <id>.jsonl, anything under the <id>/ directory, or a rollout whose name ends in -<id>.jsonl.
func matchesSession(f fileRef, id string) bool {
	if id == "" {
		return true
	}
	switch f.Root {
	case RootClaudeProjects:
		return f.SessionID == id
	case RootCodexSessions:
		return strings.HasSuffix(f.Rel, "-"+id+".jsonl")
	}
	return false
}

// fileKeyShape returns a file key with names replaced by placeholders, safe to print (the "file key
// shape" of `dead list`).
func fileKeyShape(key string) string {
	root, rel, _ := strings.Cut(key, "/")
	f, ok := classify(root, rel)
	if !ok {
		return root + "/<other>"
	}
	switch f.Role {
	case RoleMain:
		return root + "/<project>/<session>.jsonl"
	case RoleSubagent:
		return root + "/<project>/<session>/subagents/agent-<id>.jsonl"
	case RoleWorkflowAgent:
		return root + "/<project>/<session>/subagents/workflows/wf_<id>/agent-<id>.jsonl"
	case RoleWorkflowJournal:
		return root + "/<project>/<session>/subagents/workflows/wf_<id>/journal.jsonl"
	case FileAgentMeta:
		return root + "/<project>/<session>/.../agent-<id>.meta.json"
	case FileWorkflowRun:
		return root + "/<project>/<session>/workflows/wf_<id>.json"
	case FileWorkflowScript:
		return root + "/<project>/<session>/workflows/scripts/<name>.js"
	case FileToolResult:
		return root + "/<project>/<session>/tool-results/<name>"
	case RoleCodexRollout:
		return root + "/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl"
	case FileSkill:
		return root + "/<skill>/<file>"
	}
	return root + "/<other>"
}

func mtimeOf(f fileRef) time.Time { return time.Unix(0, f.MtimeNS).UTC() }
