package ct

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// DevContainerConfig is the subset of the devcontainer.json spec Corral's
// scoped MVP understands. Full spec support (Features, build.dockerfile,
// postStartCommand/postAttachCommand, mounts, an object-form
// postCreateCommand running several named commands in parallel, and
// recognition by the devcontainer CLI / VS Code's "Reopen in Container" UI)
// is tracked separately — see the "devcontainer: full spec support" issue.
type DevContainerConfig struct {
	// Image is an OCI ref to boot the CT from directly.
	Image string `json:"image"`
	// Build, if set instead of Image, means this devcontainer.json builds a
	// Dockerfile rather than pulling a ready image — not supported yet.
	Build *struct {
		Dockerfile string `json:"dockerfile"`
	} `json:"build"`
	// PostCreateCommandRaw holds postCreateCommand exactly as JSON — a
	// string (shell script), a []string (argv, no shell), or an object
	// (several named commands, run in parallel) — so ResolvePostCreate can
	// tell the three apart and give a precise error for the unsupported one.
	PostCreateCommandRaw json.RawMessage `json:"postCreateCommand"`
	// PostStartCommandRaw holds postStartCommand — same JSON shapes as
	// postCreateCommand. Runs after postCreateCommand when the CT is ready.
	// (postAttachCommand isn't supported: Corral CTs don't track attach
	// events — only creation/start. The hook would trigger unpredictably.)
	PostStartCommandRaw json.RawMessage `json:"postStartCommand"`
	RemoteUser          string          `json:"remoteUser"`
	ForwardPorts        []flexPort      `json:"forwardPorts"`
}

// flexPort accepts devcontainer.json's forwardPorts entries in either form
// the spec allows: a bare port number, or "8080:8080" (host:container) —
// Corral only has one port to expose, so the container-side (second) number
// is what's used.
type flexPort int

var hostContainerPort = regexp.MustCompile(`^\s*\d+\s*:\s*(\d+)\s*$`)

func (p *flexPort) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*p = flexPort(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("forwardPorts entry %s is neither a number nor a string", data)
	}
	if m := hostContainerPort.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("forwardPorts entry %q is not a port number", s)
	}
	*p = flexPort(n)
	return nil
}

// FindDevContainerJSON resolves the devcontainer.json a --devcontainer <path>
// flag might point at: the file itself, or a directory to search in the
// conventional order (.devcontainer/devcontainer.json, then
// .devcontainer.json at its root).
func FindDevContainerJSON(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("--devcontainer %s: %w", path, err)
	}
	if !info.IsDir() {
		return path, nil
	}
	for _, candidate := range []string{
		filepath.Join(path, ".devcontainer", "devcontainer.json"),
		filepath.Join(path, ".devcontainer.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no .devcontainer/devcontainer.json or .devcontainer.json found under %s", path)
}

// stripJSONComments removes // line comments and /* block */ comments
// outside of string literals, and trailing commas before ] or } — the JSONC
// dialect devcontainer.json is written in (VS Code's jsonc), which
// encoding/json can't parse directly.
func stripJSONComments(src []byte) []byte {
	var out []byte
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // land on the closing '/'
		default:
			out = append(out, c)
		}
	}
	return stripTrailingCommas(out)
}

var trailingComma = regexp.MustCompile(`,(\s*[}\]])`)

func stripTrailingCommas(src []byte) []byte {
	return trailingComma.ReplaceAll(src, []byte("$1"))
}

// LoadDevContainerConfig reads and parses a devcontainer.json file (JSONC:
// // and /* */ comments, trailing commas — both common in hand-written
// devcontainer.json files despite not being strict JSON).
func LoadDevContainerConfig(path string) (*DevContainerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg DevContainerConfig
	if err := json.Unmarshal(stripJSONComments(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

// ResolvedPostCreate is a devcontainer.json postCreateCommand ready to Exec:
// exactly one of Argv/Script is set (see execArgv).
type ResolvedPostCreate struct {
	Argv   []string
	Script string
}

// ResolvePostCreate interprets postCreateCommand per its three allowed JSON
// shapes. The object form (several named commands run in parallel) isn't
// supported in this scoped MVP — see DevContainerConfig's doc comment.
func (c *DevContainerConfig) ResolvePostCreate() (*ResolvedPostCreate, error) {
	if len(c.PostCreateCommandRaw) == 0 || string(c.PostCreateCommandRaw) == "null" {
		return nil, nil
	}
	var script string
	if err := json.Unmarshal(c.PostCreateCommandRaw, &script); err == nil {
		return &ResolvedPostCreate{Script: script}, nil
	}
	var argv []string
	if err := json.Unmarshal(c.PostCreateCommandRaw, &argv); err == nil {
		return &ResolvedPostCreate{Argv: argv}, nil
	}
	return nil, fmt.Errorf(
		"postCreateCommand as an object (multiple parallel named commands) isn't supported yet — " +
			"use a single string or a [\"argv\", \"form\"] array")
}

// ResolvePostStart interprets postStartCommand per its three allowed JSON
// shapes. The object form (several named commands run in parallel) isn't
// supported yet.
func (c *DevContainerConfig) ResolvePostStart() (*ResolvedPostCreate, error) {
	if len(c.PostStartCommandRaw) == 0 || string(c.PostStartCommandRaw) == "null" {
		return nil, nil
	}
	var script string
	if err := json.Unmarshal(c.PostStartCommandRaw, &script); err == nil {
		return &ResolvedPostCreate{Script: script}, nil
	}
	var argv []string
	if err := json.Unmarshal(c.PostStartCommandRaw, &argv); err == nil {
		return &ResolvedPostCreate{Argv: argv}, nil
	}
	return nil, fmt.Errorf(
		"postStartCommand as an object (multiple parallel named commands) isn't supported yet — " +
			"use a single string or a [\"argv\", \"form\"] array")
}

// Ports returns ForwardPorts as plain ints, for ApplyProxy.
func (c *DevContainerConfig) Ports() []int {
	ports := make([]int, len(c.ForwardPorts))
	for i, p := range c.ForwardPorts {
		ports[i] = int(p)
	}
	return ports
}

// WithUser wraps a resolved postCreateCommand to run as user via `su`
// (devcontainer.json's remoteUser) — a no-op if user is empty. Collapses
// the argv form into a shell-quoted script, since `su -c` only takes one.
func (r *ResolvedPostCreate) WithUser(user string) *ResolvedPostCreate {
	if user == "" || r == nil {
		return r
	}
	script := r.Script
	if len(r.Argv) > 0 {
		quoted := make([]string, len(r.Argv))
		for i, a := range r.Argv {
			quoted[i] = shellQuote(a)
		}
		script = strings.Join(quoted, " ")
	}
	return &ResolvedPostCreate{Script: fmt.Sprintf("su %s -c %s", shellQuote(user), shellQuote(script))}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
