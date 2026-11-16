package main

// The map commands: serve, html, ls, map, diff, layout, unclassified. The
// drawing itself is internal/tfmap's.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/tfmap"
	"github.com/askrike/desprawl/providers"
)

// mapFlags are the flags every map command takes.
type mapFlags struct{ layouts string }

func (o *mapFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.layouts, "layouts", "", "directory of saved layouts (default: the workspace's .desprawl/layouts)")
}

// mapLoader loads the configurations of the workspace ws with the catalogs
// desprawl ships, which those in its packs add to or replace type by type.
func mapLoader(ws string) tfmap.Loader {
	return tfmap.NewLoader(providers.Catalogs, os.DirFS(paths.Pack(ws)))
}

// site is the current workspace's map, as serve and html show it.
func (o mapFlags) site() site {
	return newSite(".", mapLoader("."), tfmap.WorkspaceLayouts(".", o.layouts))
}

// config loads the one configuration in dir and says where its layout is.
// Parse errors go to stderr, as they'd otherwise only show as missing
// components.
func (o mapFlags) config(dir string) (*tfmap.Graph, string, error) {
	if !isDir(dir) {
		return nil, "", fmt.Errorf("%s is not a directory", dir)
	}
	path := tfmap.WorkspaceLayouts(".", o.layouts).Path(dir)
	g, err := mapLoader(".")(dir, path)
	if err != nil {
		return nil, "", err
	}
	for _, d := range g.Diags {
		fmt.Fprintln(os.Stderr, "desprawl:", d)
	}
	if len(g.Nodes) == 0 {
		return nil, "", errors.New("no Terraform in " + dir)
	}
	return g, path, nil
}

const nothingToDraw = "the workspace has nothing to draw yet: add a directory of Terraform to draw in desprawl.hcl (.desprawl/imported for the imported Terraform)"

// workspaceConfig is the configuration a map command works on: arg, one of
// the current workspace's (`desprawl ls` lists them), or its first.
func workspaceConfig(name, arg string) (string, error) {
	if !model.IsWorkspace(".") {
		return "", fmt.Errorf("desprawl %s works on the workspace in the current directory (-C DIR for another): %w", name, model.NotWorkspace("."))
	}
	if _, err := model.LoadConfig("."); err != nil {
		return "", err
	}
	warnDraw()
	configs := tfmap.Configs(".")
	if len(configs) == 0 {
		return "", errors.New(nothingToDraw)
	}
	if arg == "." || arg == "" {
		return configs[0], nil
	}
	if name, err := tfmap.ConfigNameIn(".", arg); err == nil {
		arg = name
	}
	if !slices.Contains(configs, arg) {
		return "", fmt.Errorf("%s is not one of the workspace's configurations: %s", arg, strings.Join(configs, ", "))
	}
	return arg, nil
}

// workspaceRoot checks that a command drawing the whole workspace got no
// directory: it works on the current one.
func workspaceRoot(name, dir string) error {
	if dir != "." {
		return fmt.Errorf("desprawl %s works on the workspace in the current directory; for another, desprawl -C %s %s", name, dir, name)
	}
	if _, err := model.LoadConfig("."); err != nil {
		return err // not a workspace, or desprawl.hcl can't be used
	}
	warnDraw()
	return nil
}

// warnDraw tells about draw entries that draw nothing, which would
// otherwise just be missing from the map.
func warnDraw() {
	for _, p := range tfmap.DrawProblems(".") {
		fmt.Fprintln(os.Stderr, "desprawl: desprawl.hcl:", p)
	}
}

func cmdServe(args []string) error {
	var o mapFlags
	var addr string
	var open bool
	explicitAddr := false
	dir := parseArgs("serve", args, func(fs *flag.FlagSet) {
		o.register(fs)
		fs.Func("addr", "listen address (default localhost:8080, or any free port if that one is taken)", func(s string) error {
			addr, explicitAddr = s, true
			return nil
		})
		fs.BoolVar(&open, "open", true, "open the browser")
	}, ".")
	if !explicitAddr {
		addr = "localhost:8080"
	}
	if err := workspaceRoot("serve", dir); err != nil {
		return err
	}
	return o.site().serve(addr, explicitAddr, open, View{})
}

func cmdHTML(args []string) error {
	var o mapFlags
	var out string
	dir := parseArgs("html", args, func(fs *flag.FlagSet) {
		o.register(fs)
		fs.StringVar(&out, "o", "", "the file to write (default: standard output)")
	}, ".")
	if err := workspaceRoot("html", dir); err != nil {
		return err
	}
	page, err := o.site().static()
	if err != nil {
		return err
	}
	if out == "" {
		_, err = os.Stdout.Write(page)
		return err
	}
	if err := os.WriteFile(out, page, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
	return nil
}

func cmdList(args []string) error {
	dir := parseArgs("ls", args, nil, ".")
	if err := workspaceRoot("ls", dir); err != nil {
		return err
	}
	dirs := tfmap.Configs(dir)
	if len(dirs) == 0 {
		return errors.New(nothingToDraw)
	}
	for _, d := range dirs {
		fmt.Println(d)
	}
	return nil
}

// mapJSON is `desprawl map -json`: the architecture without routes, and
// the grid it's drawn on.
type mapJSON struct {
	Config       string            `json:"config"`
	LayoutFile   string            `json:"layout_file"`
	LayoutSaved  bool              `json:"layout_saved"`
	Grid         map[string][2]int `json:"grid"`
	Components   []tfmap.ArchNode  `json:"components"`
	Connections  []tfmap.ArchEdge  `json:"connections"`
	Unclassified []string          `json:"unclassified,omitempty"`
	Diagnostics  []string          `json:"diagnostics,omitempty"`
}

func cmdMap(args []string) error {
	var o mapFlags
	var asJSON bool
	arg := parseArgs("map", args, func(fs *flag.FlagSet) {
		o.register(fs)
		fs.BoolVar(&asJSON, "json", false, "print the components, connections and grid as JSON")
	}, ".")
	dir, err := workspaceConfig("map", arg)
	if err != nil {
		return err
	}
	g, path, err := o.config(dir)
	if err != nil {
		return err
	}
	if !asJSON {
		fmt.Print(tfmap.Describe(dir, g, path))
		return nil
	}
	out := mapJSON{Config: dir, LayoutFile: path, LayoutSaved: g.Layout != nil, Components: g.Arch.Nodes, Diagnostics: g.Diags}
	l := g.Layout
	if l == nil {
		l = tfmap.Seed(g.Arch)
	}
	out.Grid = l.Grid
	for _, e := range g.Arch.Edges {
		e.Route = nil
		out.Connections = append(out.Connections, e)
	}
	for _, n := range tfmap.Unclassified(g) {
		out.Unclassified = append(out.Unclassified, n.Type)
	}
	return printJSON(out)
}

func cmdDiff(args []string) error {
	var o mapFlags
	var base, addr string
	var asJSON, open bool
	arg := parseArgs("diff", args, func(fs *flag.FlagSet) {
		o.register(fs)
		fs.StringVar(&base, "base", "HEAD", "git revision to compare with (HEAD: uncommitted changes)")
		fs.BoolVar(&asJSON, "json", false, "print the changes as JSON marks (id, kind, attrs, diff)")
		fs.BoolVar(&open, "open", false, "also serve the map with the changes marked, and open it")
		fs.StringVar(&addr, "addr", "localhost:8080", "listen address with -open")
	}, ".")
	id, err := workspaceConfig("diff", arg)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(id) // Since finds the config in the repository by its path
	if err != nil {
		return err
	}
	g, _, err := o.config(dir)
	if err != nil {
		return err
	}
	ch, err := tfmap.Since(dir, g, base, mapLoader("."))
	if err != nil {
		return err
	}
	if asJSON {
		if err := printJSON(ch.Marks); err != nil {
			return err
		}
	} else {
		fmt.Print(ch.Summary)
	}
	if !open {
		return nil
	}
	v := View{Dir: id, Title: "Changes since " + base, Marks: ch.Marks}
	if len(ch.Marks) == 0 {
		v.Note = "Nothing in this configuration differs from " + base + "."
	}
	return o.site().serve(addr, false, true, v)
}

func cmdLayout(args []string) error {
	var o mapFlags
	if len(args) == 0 || (args[0] != "seed" && args[0] != "tidy") {
		fs := newFlagSet("layout")
		o.register(fs)
		fs.Usage()
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
			os.Exit(0)
		}
		os.Exit(2)
	}
	op := args[0]
	arg := parseArgs("layout", args[1:], o.register, ".")
	dir, err := workspaceConfig("layout", arg)
	if err != nil {
		return err
	}
	g, path, err := o.config(dir)
	if err != nil {
		return err
	}
	l := tfmap.Seed(g.Arch)
	if op == "tidy" {
		if g.Layout == nil {
			return fmt.Errorf("no saved layout at %s: run desprawl layout seed first", path)
		}
		l = g.Layout
		tfmap.Tidy(g.Arch, l)
	}
	changed, err := tfmap.SaveLayout(path, *l)
	if err != nil {
		return err
	}
	g.SetLayout(l)
	if changed {
		fmt.Println("wrote", path)
	} else {
		fmt.Println("unchanged:", path)
	}
	fmt.Println()
	fmt.Print(tfmap.RenderASCII(g.Arch, l))
	return nil
}

func cmdUnclassified(args []string) error {
	var o mapFlags
	arg := parseArgs("unclassified", args, o.register, ".")
	dir, err := workspaceConfig("unclassified", arg)
	if err != nil {
		return err
	}
	g, _, err := o.config(dir)
	if err != nil {
		return err
	}
	for _, n := range tfmap.Unclassified(g) {
		fmt.Printf("%-8s %-50s guessed=%s\n", n.Kind, n.Type, n.Role)
	}
	return nil
}
