package tfmap

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/paths"
)

// Mark annotates one thing on the map: a component id, a resource id (in
// the architecture view, drawn on the component it's folded into), or a
// connection "from -> to" between two component ids.
type Mark struct {
	ID    string   `json:"id" jsonschema:"component id, resource id (e.g. aws_iam_role.x), or a connection as \"<from id> -> <to id>\""`
	Kind  string   `json:"kind,omitempty" jsonschema:"how to draw it: highlight (default), added, changed, removed or warning"`
	Note  string   `json:"note,omitempty" jsonschema:"short text shown on the map next to it (a few words) and in full in the side panel"`
	Attrs []string `json:"attrs,omitempty" jsonschema:"attributes this is about (e.g. the ones that changed); highlighted in the component's settings"`
	Diff  string   `json:"diff,omitempty" jsonschema:"optional unified-diff-style text (lines starting with +, - or space) shown in the side panel"`
	Owner string   `json:"owner,omitempty" jsonschema:"for something no longer on the map (a removed resource): the component id it belonged to, which gets the mark instead"`
}

// Changes are a configuration's differences from its version at a git
// revision, block by block: which resources were added, removed or changed,
// which attributes changed, and which connections appeared or went.
type Changes struct {
	Marks   []Mark
	Summary string
}

// Since compares the configuration in dir, loaded as g, with its .tf
// files at the git revision base.
func Since(dir string, g *Graph, base string, load Loader) (Changes, error) {
	oldDir, cleanup, err := gitTree(dir, base)
	if err != nil {
		return Changes{}, err
	}
	defer cleanup()
	old := &Graph{}
	if isDir(oldDir) {
		if old, err = load(oldDir, ""); err != nil {
			return Changes{}, fmt.Errorf("reading %s at %s: %w", dir, base, err)
		}
	}
	return diffGraphs(old, g, base), nil
}

// gitTree writes the Terraform files of the repository holding dir, as they
// are at rev, into a temporary directory, and returns where dir is in it.
// Only .tf, .tf.json and .tfvars files are written; symlinks are skipped.
func gitTree(dir, rev string) (old string, cleanup func(), err error) {
	top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", nil, fmt.Errorf("%s is not in a git repository: pass marks to show_map instead", dir)
	}
	repo := strings.TrimSpace(string(top))
	var stderr bytes.Buffer
	cmd := exec.Command("git", "-C", repo, "archive", "--format=tar", rev)
	cmd.Stderr = &stderr
	archive, err := cmd.Output()
	if err != nil {
		return "", nil, fmt.Errorf("git archive %s: %s", rev, strings.TrimSpace(stderr.String()))
	}
	tmp, err := paths.Scratch()
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return "", nil, err
		}
		name := filepath.FromSlash(h.Name)
		if h.Typeflag != tar.TypeReg || !(strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tfvars")) ||
			!filepath.IsLocal(name) {
			continue
		}
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			cleanup()
			return "", nil, err
		}
		b, err := io.ReadAll(tr)
		if err == nil {
			err = os.WriteFile(path, b, 0o644)
		}
		if err != nil {
			cleanup()
			return "", nil, err
		}
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	realDir, _ := filepath.EvalSymlinks(dir)
	rel, err := filepath.Rel(realRepo, realDir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	old = filepath.Join(tmp, rel)
	// Installed registry modules aren't in git: use today's, so they don't
	// all look added.
	if isDir(filepath.Join(dir, ".terraform")) && isDir(old) {
		os.Symlink(filepath.Join(dir, ".terraform"), filepath.Join(old, ".terraform"))
	}
	return old, cleanup, nil
}

// diffGraphs compares two versions of a configuration.
func diffGraphs(old, cur *Graph, rev string) Changes {
	oldNodes := map[string]Node{}
	for _, n := range old.Nodes {
		oldNodes[n.ID] = n
	}
	curNodes := map[string]bool{}
	ownerIn := func(a Arch, id string) string {
		for _, c := range a.Nodes {
			if slices.Contains(c.Members, id) {
				return c.ID
			}
		}
		return ""
	}
	var marks []Mark
	var lines []string
	var added, changed, removed int
	for _, n := range cur.Nodes {
		curNodes[n.ID] = true
		o, ok := oldNodes[n.ID]
		if !ok {
			added++
			marks = append(marks, Mark{ID: n.ID, Kind: "added", Diff: blockDiff("", n.Source)})
			lines = append(lines, "  + "+n.ID+inComponent(ownerIn(cur.Arch, n.ID), n.ID))
			continue
		}
		if normalize(o.Source) == normalize(n.Source) {
			continue
		}
		changed++
		attrs := changedAttrs(o.Source, n.Source)
		marks = append(marks, Mark{ID: n.ID, Kind: "changed", Attrs: attrs, Diff: blockDiff(o.Source, n.Source)})
		what := ""
		if len(attrs) > 0 {
			what = ": " + strings.Join(attrs, ", ")
		}
		lines = append(lines, "  ~ "+n.ID+what+inComponent(ownerIn(cur.Arch, n.ID), n.ID))
	}
	for _, o := range old.Nodes {
		if curNodes[o.ID] {
			continue
		}
		removed++
		owner := ownerIn(old.Arch, o.ID)
		marks = append(marks, Mark{ID: o.ID, Kind: "removed", Owner: owner, Diff: blockDiff(o.Source, "")})
		lines = append(lines, "  - "+o.ID+inComponent(owner, o.ID))
	}

	key := func(e ArchEdge) string { return e.From + " -> " + e.To + " (" + e.Label + ")" }
	oldEdges, curEdges := map[string]bool{}, map[string]bool{}
	for _, e := range old.Arch.Edges {
		oldEdges[key(e)] = true
	}
	var conns []string
	for _, e := range cur.Arch.Edges {
		curEdges[key(e)] = true
		if !oldEdges[key(e)] {
			marks = append(marks, Mark{ID: e.From + " -> " + e.To, Kind: "added", Note: e.Label})
			conns = append(conns, "  + "+key(e))
		}
	}
	for _, e := range old.Arch.Edges {
		if !curEdges[key(e)] {
			marks = append(marks, Mark{ID: e.From + " -> " + e.To, Kind: "removed", Note: e.Label})
			conns = append(conns, "  - "+key(e))
		}
	}

	var b strings.Builder
	if added+changed+removed+len(conns) == 0 {
		fmt.Fprintf(&b, "no changes to %s since %s\n", cur.Dir, rev)
		return Changes{Summary: b.String()}
	}
	fmt.Fprintf(&b, "changes to %s since %s: %d added, %d changed, %d removed\n", cur.Dir, rev, added, changed, removed)
	b.WriteString(strings.Join(lines, "\n") + "\n")
	if len(conns) > 0 {
		b.WriteString("connections:\n" + strings.Join(conns, "\n") + "\n")
	}
	return Changes{Marks: marks, Summary: b.String()}
}

func inComponent(owner, id string) string {
	if owner == "" || owner == id {
		return ""
	}
	return " (in " + owner + ")"
}

// normalize ignores formatting: blank lines and runs of spaces.
func normalize(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			out = append(out, strings.Join(f, " "))
		}
	}
	return strings.Join(out, "\n")
}

// diffLine is one line of a block diff: op is ' ', '+' or '-'; key is the
// top-level attribute or nested block the line belongs to.
type diffLine struct {
	op   byte
	text string
	key  string
}

var keyLine = regexp.MustCompile(`^\s*([A-Za-z_][\w-]*)\s*(?:=|\{|"[^"]*"\s*\{)`)

// keyedLines splits a block's source into non-blank lines, each with the
// top-level attribute or nested block it is part of ("" for the block's own
// header and closing brace).
func keyedLines(src string) (lines, keys []string) {
	depth, key := 0, ""
	for _, l := range strings.Split(src, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if depth == 1 {
			key = ""
			if m := keyLine.FindStringSubmatch(l); m != nil {
				key = m[1]
			}
		}
		if depth == 0 {
			key = ""
		}
		lines, keys = append(lines, l), append(keys, key)
		depth += strings.Count(l, "{") + strings.Count(l, "[") + strings.Count(l, "(") -
			strings.Count(l, "}") - strings.Count(l, "]") - strings.Count(l, ")")
		if depth == 0 { // a closing brace ends the block's own key
			key = ""
		}
	}
	return lines, keys
}

// lineDiff is a longest-common-subsequence diff of two blocks, comparing
// lines with formatting ignored. Blocks are small, so quadratic is fine.
func lineDiff(a, b string) []diffLine {
	al, ak := keyedLines(a)
	bl, bk := keyedLines(b)
	na, nb := make([]string, len(al)), make([]string, len(bl))
	for i, l := range al {
		na[i] = strings.Join(strings.Fields(l), " ")
	}
	for i, l := range bl {
		nb[i] = strings.Join(strings.Fields(l), " ")
	}
	lcs := make([][]int, len(al)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(bl)+1)
	}
	for i := len(al) - 1; i >= 0; i-- {
		for j := len(bl) - 1; j >= 0; j-- {
			if na[i] == nb[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < len(al) || j < len(bl) {
		switch {
		case i < len(al) && j < len(bl) && na[i] == nb[j]:
			out = append(out, diffLine{' ', bl[j], bk[j]})
			i, j = i+1, j+1
		case j < len(bl) && (i == len(al) || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, diffLine{'+', bl[j], bk[j]})
			j++
		default:
			out = append(out, diffLine{'-', al[i], ak[i]})
			i++
		}
	}
	return out
}

// changedAttrs lists the top-level attributes and nested blocks whose lines
// differ, in order.
func changedAttrs(a, b string) []string {
	var keys []string
	for _, l := range lineDiff(a, b) {
		if l.op != ' ' && l.key != "" && !slices.Contains(keys, l.key) {
			keys = append(keys, l.key)
		}
	}
	return keys
}

// blockDiff renders a block diff, keeping two lines of context around
// changes and the block's header.
func blockDiff(a, b string) string {
	lines := lineDiff(a, b)
	keep := make([]bool, len(lines))
	for i, l := range lines {
		if l.op != ' ' {
			for k := max(0, i-2); k <= min(len(lines)-1, i+2); k++ {
				keep[k] = true
			}
		}
	}
	if len(keep) > 0 {
		keep[0] = true
	}
	var out strings.Builder
	skipped := false
	for i, l := range lines {
		if !keep[i] {
			skipped = true
			continue
		}
		if skipped {
			out.WriteString("  …\n")
			skipped = false
		}
		out.WriteByte(l.op)
		out.WriteString(" " + l.text + "\n")
	}
	if skipped {
		out.WriteString("  …\n")
	}
	return out.String()
}
