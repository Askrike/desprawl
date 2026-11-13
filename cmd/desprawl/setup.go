package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/askrike/desprawl/skills"
)

// harnesses.json lists the agent harnesses setup knows: a directory whose
// existence means the harness is installed, where it reads user-level
// skills, and how to register an MCP server with it: commands to run
// ("run", {exe} is this binary), a JSON file with an "mcpServers" object
// ("json"), or a TOML file with [mcp_servers.<name>] tables ("toml").
//
//go:embed harnesses.json
var harnessesJSON []byte

type harness struct {
	Name   string     `json:"name"`
	Detect string     `json:"detect"`
	Skills string     `json:"skills"`
	Run    [][]string `json:"run,omitempty"`
	JSON   string     `json:"json,omitempty"`
	TOML   string     `json:"toml,omitempty"`
}

func cmdSetup(args []string) error {
	fl := newFlagSet("setup")
	printOnly := fl.Bool("print", false, "print the MCP config and skills location for other harnesses; change nothing")
	skillsTo := fl.String("skills", "", "only write the skills into this directory (for harnesses setup doesn't know)")
	fl.Parse(args)
	if *skillsTo != "" {
		names, err := installSkills(*skillsTo)
		if err != nil {
			return err
		}
		fmt.Printf("✓ skills %s -> %s\n", strings.Join(names, ", "), *skillsTo)
		return nil
	}

	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	if *printOnly {
		names, _ := skillNames()
		fmt.Printf("MCP server (most harnesses take this shape):\n\n%s\n\nSkills (%s): write them into the harness's skills directory with\n  desprawl setup -skills ~/.agents/skills\n", mcpSnippet(exe), strings.Join(names, ", "))
		return nil
	}
	if strings.Contains(exe, "go-build") {
		return fmt.Errorf("%s is a temporary go run build: build or install desprawl first, then run its setup", exe)
	}

	var hs []harness
	if err := json.Unmarshal(harnessesJSON, &hs); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	expand := func(p string) string {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			return filepath.Join(home, filepath.FromSlash(rest))
		}
		return p
	}

	found := 0
	skillDirs := map[string]bool{}
	for _, h := range hs {
		if !isDir(expand(h.Detect)) {
			continue
		}
		found++
		var err error
		switch {
		case len(h.Run) > 0:
			err = runAll(h.Run, exe)
		case h.JSON != "":
			err = addJSONServer(expand(h.JSON), exe)
		case h.TOML != "":
			err = addTOMLServer(expand(h.TOML), exe)
		}
		if err != nil {
			fmt.Printf("✗ %s: %v\n", h.Name, err)
			continue
		}
		fmt.Printf("✓ %s: MCP server added\n", h.Name)
		skillDirs[expand(h.Skills)] = true
	}
	if found == 0 {
		fmt.Println("no known agent harness found; `desprawl setup -print` shows the config to add by hand")
	}
	for _, d := range slices.Sorted(maps.Keys(skillDirs)) {
		names, err := installSkills(d)
		if err != nil {
			return err
		}
		fmt.Printf("✓ skills %s -> %s\n", strings.Join(names, ", "), d)
	}

	// The skills run desprawl's commands through the shell.
	if p, err := exec.LookPath("desprawl"); err != nil {
		fmt.Printf("\n! desprawl is not on your PATH; the skills call it by name. Move %s into a directory on your PATH and run setup again.\n", exe)
	} else if p, _ = filepath.EvalSymlinks(p); p != exe {
		fmt.Printf("\n! `desprawl` on your PATH is %s, not this binary (%s).\n", p, exe)
	}
	if found > 0 {
		fmt.Println("\nRestart your agent, then ask it: \"generate a cloud map of my infrastructure using desprawl\"")
	}
	return nil
}

func mcpServer(exe string) map[string]any {
	return map[string]any{"command": exe, "args": []string{"mcp"}}
}

func mcpSnippet(exe string) string {
	b, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"desprawl": mcpServer(exe)}}, "", "  ")
	return string(b)
}

func runAll(cmds [][]string, exe string) error {
	for i, c := range cmds {
		args := make([]string, len(c))
		for j, a := range c {
			args[j] = strings.ReplaceAll(a, "{exe}", exe)
		}
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		// Every command but the last may fail: removing a server that isn't
		// there yet, say.
		if err != nil && i == len(cmds)-1 {
			return fmt.Errorf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// backup copies path to path.bak before setup changes it.
func backup(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b, os.WriteFile(path+".bak", b, 0o600)
}

// addJSONServer sets mcpServers.desprawl in a JSON config file. Files it
// can't parse (comments, say) are left alone.
func addJSONServer(path, exe string) error {
	old, err := backup(path)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	if len(old) > 0 {
		d := json.NewDecoder(bytes.NewReader(old))
		d.UseNumber() // keep numbers as written: float64 rounds large ones
		if err := d.Decode(&cfg); err != nil {
			return fmt.Errorf("can't parse %s (%v); add this yourself:\n%s", path, err, mcpSnippet(exe))
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers["desprawl"] = mcpServer(exe)
	cfg["mcpServers"] = servers
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// tomlTable matches the [mcp_servers.desprawl] table up to the next table
// header. Its subtables ([mcp_servers.desprawl.env]) are the user's and stay.
var tomlTable = regexp.MustCompile(`(?m)^[ \t]*\[[ \t]*mcp_servers[ \t]*\.[ \t]*("desprawl"|desprawl)[ \t]*\][ \t]*(#.*)?\r?\n(([ \t]*[^\[\s].*)?\r?\n)*`)

// tomlOther finds desprawl defined some other way (an inline table under
// [mcp_servers], a dotted key), which a second table would clash with.
var tomlOther = regexp.MustCompile(`(?m)^[ \t]*(mcp_servers[ \t]*\.[ \t]*)?("desprawl"|desprawl)[ \t]*=`)

// addTOMLServer replaces or appends the [mcp_servers.desprawl] table.
func addTOMLServer(path, exe string) error {
	old, err := backup(path)
	if err != nil {
		return err
	}
	table := fmt.Sprintf("[mcp_servers.desprawl]\ncommand = %q\nargs = [\"mcp\"]\n", exe)
	s := tomlTable.ReplaceAllString(string(old)+"\n", "")
	if tomlOther.MatchString(s) {
		return fmt.Errorf("%s already defines desprawl in a form setup can't edit; replace it with:\n%s", path, table)
	}
	s = strings.TrimRight(s, "\r\n")
	if s != "" {
		s += "\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(s+table), 0o600)
}

func skillNames() ([]string, error) {
	entries, err := fs.ReadDir(skills.FS, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// installSkills replaces desprawl's skills in dir with this binary's, so
// skills always match the binary that serves their tools.
func installSkills(dir string) ([]string, error) {
	names, err := skillNames()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		target := filepath.Join(dir, name)
		if err := os.RemoveAll(target); err != nil {
			return nil, err
		}
		err := fs.WalkDir(skills.FS, name, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := skills.FS.ReadFile(p)
			if err != nil {
				return err
			}
			out := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		})
		if err != nil {
			return nil, err
		}
	}
	return names, nil
}
