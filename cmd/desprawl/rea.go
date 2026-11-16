package main

// The reverse-engineering commands: they parse flags and print; the work is
// in internal/rea. See README.md and the desprawl-reverse-engineer skill for
// the workflow.

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/askrike/desprawl/internal/discover"
	"github.com/askrike/desprawl/internal/link"
	"github.com/askrike/desprawl/internal/mine"
	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/rea"
	"github.com/askrike/desprawl/internal/rules"
	"github.com/askrike/desprawl/internal/tf"
	"github.com/askrike/desprawl/internal/tfmap"
)

func cmdInit(args []string) error {
	var source, version, env, output, draw string
	var configs lines
	dir := flags("init", args, func(fs *flag.FlagSet) {
		fs.StringVar(&source, "provider", "", "provider source to reverse-engineer with, e.g. hashicorp/aws (leave out to only draw)")
		fs.StringVar(&version, "version", "", "provider version constraint (default: latest)")
		fs.Var(&configs, "config", "one of the provider's settings, as HCL, e.g. 'region = \"eu-central-1\"' (repeat for each)")
		fs.StringVar(&env, "env", "", "environment for terraform, e.g. AWS_PROFILE=dev,GOOGLE_PROJECT=x (no secrets: desprawl.hcl is committed)")
		fs.StringVar(&output, "output", "", "where the imported Terraform goes (default "+paths.DefaultOutput+")")
		fs.StringVar(&draw, "draw", "", "Terraform directories to draw, comma-separated (with -provider, the imported Terraform is added)")
	})
	config := strings.Join(configs, "\n")
	given := source+version+config+env+output+draw != ""
	// In a workspace, init rewrites desprawl.hcl: from the flags, or from
	// the answers, which start from what the file says now. Without either
	// (no terminal), it sets terraform up again for the file as it is, like
	// terraform init.
	var cur *model.Config
	if model.IsWorkspace(*dir) {
		cfg, err := model.LoadConfig(*dir)
		if err != nil && (given || !isTerminal(os.Stdin)) {
			return err
		}
		if err == nil {
			cur = &cfg
		}
		if !given && !isTerminal(os.Stdin) {
			if len(cfg.Providers) == 0 {
				rea.Log.Info("this workspace only draws (desprawl.hcl has no provider block): nothing to set up")
				return nil
			}
			return rea.Init(*dir)
		}
	}
	var ps []model.Provider
	drawDirs := split(draw)
	switch {
	case !given && isTerminal(os.Stdin):
		// Nothing said on the command line, and someone to ask.
		var err error
		if ps, drawDirs, output, err = askInit(os.Stdin, os.Stderr, cur); err != nil {
			return err
		}
	case !given:
		return errors.New("nothing to set up: pass -provider NS/NAME to reverse-engineer an account, -draw DIR to draw existing Terraform, or both (init only asks when run in a terminal)")
	case source != "":
		p, err := rea.NewProvider(source, version, config, split(env))
		if err != nil {
			return err
		}
		ps = append(ps, p)
	case version != "" || config != "" || env != "":
		return errors.New("-version, -config and -env describe a provider: pass -provider NS/NAME with them")
	}
	if source != "" {
		// The imported Terraform is drawn only if draw lists it.
		drawDirs = withDir(drawDirs, cmp.Or(output, paths.DefaultOutput))
	}
	src := rea.RenderConfig(ps, drawDirs, output)
	// Check what's written reads back, so a bad answer leaves no workspace.
	if _, err := model.ParseConfig(src, paths.ConfigFile(*dir)); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(paths.ConfigFile(*dir), src, 0o644); err != nil {
		return err
	}
	if cur != nil {
		rea.Log.Info("rewrote %s (comments and anything init doesn't ask about are gone)", paths.ConfigFile(*dir))
	} else {
		rea.Log.Info("wrote %s: edit it to change anything init set", paths.ConfigFile(*dir))
	}
	if len(ps) == 0 {
		return nil
	}
	if err := rea.Init(*dir); err != nil {
		return fmt.Errorf("%w\n(desprawl.hcl is written: fix the provider there, then run `desprawl init` again)", err)
	}
	return nil
}

// lines is a flag that may be given more than once, each value a line.
type lines []string

func (l *lines) String() string     { return strings.Join(*l, "\n") }
func (l *lines) Set(s string) error { *l = append(*l, s); return nil }

// withDir is draw with dir first, unless it's already in it.
func withDir(draw []string, dir string) []string {
	for _, d := range draw {
		if filepath.Clean(d) == filepath.Clean(dir) {
			return draw
		}
	}
	return append([]string{dir}, draw...)
}

// isTerminal reports whether f is a terminal someone can answer questions
// in. It asks the OS (a console mode on Windows, termios elsewhere), so
// /dev/null, NUL, pipes and files are not: scripts, CI and agents run
// commands with /dev/null as stdin. (Comparing f with os.DevNull doesn't
// work on Windows: a console and NUL both have no file ID, so os.SameFile
// says they're the same file and init never asked.)
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// askInit asks for what init would otherwise take as flags.
// askInit asks what desprawl.hcl should say. cur is what it says now, if
// there is one: its answers are the defaults, so Enter keeps them.
func askInit(r io.Reader, out io.Writer, cur *model.Config) (ps []model.Provider, draw []string, output string, err error) {
	in := bufio.NewReader(r)
	ask := func(q, def string) string {
		if def != "" {
			fmt.Fprintf(out, "%s [%s]: ", q, def)
		} else {
			fmt.Fprintf(out, "%s: ", q)
		}
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line == "" {
			return def
		}
		return line
	}
	yes := func(q string, def bool) bool {
		d := "y/N"
		if def {
			d = "Y/n"
		}
		a := strings.ToLower(ask(q+" ("+d+")", ""))
		return a == "y" || a == "yes" || (a == "" && def)
	}
	first := "Reverse-engineer a live cloud account?"
	if cur == nil {
		fmt.Fprintln(out, "Setting up a desprawl workspace here. Everything you answer goes into desprawl.hcl, which you can edit later.")
	} else {
		fmt.Fprintln(out, "Rewriting this workspace's desprawl.hcl. Enter keeps what it says now.")
		for _, p := range cur.Providers {
			if yes(fmt.Sprintf("Keep provider %s (%s)?", p.Ref(), p.Source), true) {
				ps = append(ps, p)
			}
		}
		first = "Add a provider or account?"
	}
	for more := yes(first, cur == nil); more; more = yes("Add another provider or account?", false) {
		source := ask("  Provider source (e.g. hashicorp/aws, hashicorp/google, cloudflare/cloudflare)", "")
		if source == "" {
			break
		}
		version := ask("  Version constraint (empty for the latest)", "")
		fmt.Fprintln(out, "  The provider's settings, as in its provider block (e.g. region = \"eu-central-1\"); an empty line ends them:")
		var body []string
		for {
			l := ask("   ", "")
			if l == "" {
				break
			}
			body = append(body, l)
		}
		q := "  Environment for terraform, NAME=value, comma-separated (e.g. AWS_PROFILE=dev; no secrets)"
		if len(ps) > 0 {
			q = "  More environment for terraform, NAME=value (shared by every block: choose this account in its settings above)"
		}
		env := ask(q, "")
		p, err := rea.NewProvider(source, version, strings.Join(body, "\n"), split(env))
		if err != nil {
			return nil, nil, "", err
		}
		for _, o := range ps {
			if o.Name == p.Name {
				p.Alias = ask("  A second "+p.Name+" block needs an alias (e.g. a region)", "")
			}
		}
		ps = append(ps, p)
	}
	// The imported Terraform is drawn only if draw lists it, so a new
	// import offers it as the first entry.
	var drawDef []string
	q := "Terraform directories to draw, comma-separated"
	if cur != nil {
		drawDef = cur.Draw
		if cur.Output != "" {
			output = cur.Output
		}
	}
	if len(ps) > 0 {
		output = ask("Where should the imported Terraform go?", cmp.Or(output, paths.DefaultOutput))
		if cur == nil || len(cur.Providers) == 0 {
			drawDef = withDir(drawDef, output)
		}
		q = "Terraform directories to draw, comma-separated: the imported Terraform's and any you have"
	}
	if len(drawDef) > 0 {
		q += " (- for none)"
	}
	if d := ask(q, strings.Join(drawDef, ", ")); d != "-" {
		draw = split(d)
	}
	if output == paths.DefaultOutput {
		output = ""
	}
	return ps, draw, output, nil
}

func cmdDiscover(args []string) error {
	var types, exclude string
	var opts discover.Options
	var asJSON bool
	dir := flags("discover", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
		fs.StringVar(&types, "types", "", "only these types (comma-separated)")
		fs.StringVar(&exclude, "exclude-types", "", "skip these types, in addition to the pack's exclusions")
		fs.IntVar(&opts.Parallel, "parallel", rea.DiscoverDefaults.Parallel, "list types queried at once")
		fs.DurationVar(&opts.Timeout, "timeout", rea.DiscoverDefaults.Timeout, "how long a round waits for a list type; a slower one keeps listing for up to 3x this")
		fs.IntVar(&opts.Limit, "limit", rea.DiscoverDefaults.Limit, "results per list block (some providers pass this to APIs capped at 1000)")
		fs.BoolVar(&opts.AllVersions, "all-versions", false, "keep every revision of versioned resources")
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	if err := w.Discover(split(types), split(exclude), opts); err != nil {
		return err
	}
	if w, err = rea.Open(*dir); err != nil {
		return err
	}
	schemas, err := w.Schemas(context.Background())
	if err != nil {
		return err
	}
	groups, err := w.DiscoverProblems(schemas)
	if err != nil {
		return err
	}
	rea.PrintProblems(os.Stdout, append(groups, w.LeakProblems(schemas)...), asJSON)
	return nil
}

func cmdAddTarget(args []string) error {
	var typ, id, identity, name, provider string
	dir := flags("add-target", args, func(fs *flag.FlagSet) {
		fs.StringVar(&provider, "provider", "", "the provider block to import it with, e.g. aws.us (default: the type's provider's first block)")
		fs.StringVar(&typ, "type", "", "terraform resource type")
		fs.StringVar(&id, "id", "", "import ID")
		fs.StringVar(&identity, "identity", "", "resource identity as JSON")
		fs.StringVar(&name, "name", "", "display name (used for the terraform name)")
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	if typ == "" || (id == "") == (identity == "") {
		return errors.New("-type and exactly one of -id or -identity are required")
	}
	if provider == "" {
		provider = w.ProviderOf(typ).Ref()
	} else if !slices.ContainsFunc(w.Config.Providers, func(p model.Provider) bool { return p.Ref() == provider }) {
		return fmt.Errorf("desprawl.hcl has no provider block %s", provider)
	}
	t := model.Target{Type: typ, ID: id, Display: name, Added: true, Provider: provider}
	if identity != "" {
		if err := json.Unmarshal([]byte(identity), &t.Identity); err != nil {
			return fmt.Errorf("-identity: %w", err)
		}
	}
	var targets []model.Target
	if err := w.ReadJSON("targets.json", &targets); err != nil && !os.IsNotExist(err) {
		return err
	}
	targets = discover.Rename(append(targets, t))
	if err := w.WriteJSON("targets.json", targets); err != nil {
		return err
	}
	rea.Log.Info("%d targets", len(targets))
	return nil
}

func cmdImportFormat(args []string) error {
	var typ string
	dir := flags("import-format", args, func(fs *flag.FlagSet) {
		fs.StringVar(&typ, "type", "", "terraform resource type")
	})
	if typ == "" {
		return errors.New("-type is required")
	}
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	p, err := w.Pack()
	if err != nil {
		return err
	}
	schemas, err := w.Schemas(context.Background())
	if err != nil {
		return err
	}
	if is := schemas.Identities[typ]; is != nil {
		fmt.Println("identity attributes (use -identity):")
		for _, name := range slices.Sorted(maps.Keys(is.Attributes)) {
			a := is.Attributes[name]
			fmt.Printf("  %s %s required=%v\n", name, a.Type, a.RequiredForImport)
		}
	}
	if ex := p.Imports[typ]; ex != nil {
		b, _ := json.MarshalIndent(ex, "", "  ")
		fmt.Printf("documented examples:\n%s\n", b)
	} else {
		fmt.Println("no documented examples in the pack (run mine-docs)")
	}
	return nil
}

func cmdGenerate(args []string) error {
	var asJSON, all bool
	dir := flags("generate", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
		fs.BoolVar(&all, "all", false, "regenerate every resource, not only those left out last time or whose override changed")
		noLLM(fs)
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	if err := w.Generate(all); err != nil {
		return err
	}
	groups, err := w.GenerateProblems()
	if err != nil {
		return err
	}
	rea.PrintProblems(os.Stdout, groups, asJSON)
	return nil
}

const rulesUsage = "rule sources, comma-separated: 'pack' (the provider's pack), JSON rule files, or 'none'"

func cmdLink(args []string) error {
	var noParam, asJSON bool
	var rulesSpec string
	dir := flags("link", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
		fs.BoolVar(&noParam, "no-parametrize", false, "keep scope parameters (account, project, region) literal")
		fs.StringVar(&rulesSpec, "rules", "pack", rulesUsage)
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	opts, err := w.LinkOptions(split(rulesSpec), !noParam)
	if err != nil {
		return err
	}
	if err := w.Link(opts); err != nil {
		return err
	}
	schemas, err := w.Schemas(context.Background())
	if err != nil {
		return err
	}
	groups, err := w.LinkProblems(schemas, opts, !noParam)
	if err != nil {
		return err
	}
	rea.PrintProblems(os.Stdout, append(groups, w.LeakProblems(schemas)...), asJSON)
	return nil
}

func cmdVerify(args []string) error {
	var asJSON bool
	dir := flags("verify", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	if err := w.Verify(); err != nil {
		return err
	}
	groups, err := w.VerifyProblems()
	if err != nil {
		return err
	}
	rea.PrintProblems(os.Stdout, groups, asJSON)
	return rea.DriftError(groups)
}

func cmdRun(args []string) error {
	var asJSON bool
	dir := flags("run", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
		noLLM(fs)
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	if err := w.Discover(nil, nil, rea.DiscoverDefaults); err != nil {
		return err
	}
	if w, err = rea.Open(*dir); err != nil {
		return err
	}
	if err := w.Generate(false); err != nil {
		return err
	}
	opts, err := w.LinkOptions([]string{"pack"}, true)
	if err != nil {
		return err
	}
	if err := w.Link(opts); err != nil {
		return err
	}
	if err := w.Verify(); err != nil {
		return err
	}
	schemas, err := w.Schemas(context.Background())
	if err != nil {
		return err
	}
	groups, err := w.AllProblems(schemas)
	if err != nil {
		return err
	}
	rea.PrintProblems(os.Stdout, groups, asJSON)
	if !asJSON {
		// What each stage did, at the end where the output's last lines
		// are read, so the counts to report need no status call.
		printStages(w)
	}
	return rea.DriftError(groups)
}

// printStages prints a line per stage as the map page's overview shows
// it, and the next step.
func printStages(w rea.Workspace) {
	v, err := rea.ViewWorkspace(w.Dir, time.Now())
	if err != nil {
		return
	}
	for _, st := range v.Stages {
		line := fmt.Sprintf("%-9s %s", st.Name, st.State)
		if len(st.Changed) > 0 {
			line += ", out of date: " + strings.Join(st.Changed, ", ") + " changed since it ran"
		}
		if st.Finished != nil {
			line += ", " + st.Finished.Local().Format("2006-01-02 15:04")
		}
		if st.Result != "" {
			line += " (" + st.Result + ")"
		}
		fmt.Println(line)
	}
	fmt.Println("next:", v.Next)
}

func cmdStatus(args []string) error {
	var asJSON bool
	dir := flags("status", args, func(fs *flag.FlagSet) {
		fs.BoolVar(&asJSON, "json", false, "print problems as one JSON object per line")
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	w.ReadOnly = true
	if len(w.Config.Providers) == 0 {
		// A workspace that only draws: what it draws is its status.
		abs, _ := filepath.Abs(w.Dir)
		fmt.Printf("workspace %s draws (no provider block, so nothing is reverse-engineered):\n", abs)
		for _, c := range tfmap.Configs(w.Dir) {
			fmt.Println("  " + c)
		}
		for _, p := range tfmap.DrawProblems(w.Dir) {
			fmt.Println("  desprawl.hcl: " + p)
		}
		return nil
	}
	rea.PrintProgress(w)
	for _, l := range w.Locations() {
		fmt.Println(l)
	}
	fmt.Println()
	printStages(w)
	// Problems need the providers' schema, which init saves; status only
	// reads, so without one there are none to show yet.
	schemas, err := tf.LoadSchemas(w.StatePath("schema.json"))
	if err != nil {
		return nil //nolint:nilerr // no schema yet: no problems to show
	}
	groups, err := w.AllProblems(schemas)
	if err != nil {
		return err
	}
	fmt.Println()
	rea.PrintProblems(os.Stdout, groups, asJSON)
	return nil
}

// cmdWhy prints the evidence for references from and/or to resources.
// Addresses match exactly or as a prefix ("aws_lambda_function.").
func cmdWhy(args []string) error {
	var from, to, conf string
	dir := flags("why", args, func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", "", "referencing resource address (or address prefix)")
		fs.StringVar(&to, "to", "", "referenced resource address (or address prefix)")
		fs.StringVar(&conf, "confidence", "", "only references with this confidence")
	})
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	var edges []link.Edge
	if err := w.ReadJSON("links.json", &edges); err != nil {
		return fmt.Errorf("%w (run link first)", err)
	}
	match := func(addr, want string) bool {
		return want == "" || addr == want || strings.HasPrefix(addr, want)
	}
	n := 0
	for _, e := range edges {
		parts := strings.SplitN(e.Ref, ".", 3) // TYPE.NAME.ATTR
		if !match(e.From, from) || !match(parts[0]+"."+parts[1], to) || (conf != "" && string(e.Confidence) != conf) {
			continue
		}
		n++
		fmt.Printf("%s.%s = %s  [%s]\n  replaced %q\n  because %s\n", e.From, e.Path, e.Ref, e.Confidence, e.Value, e.Basis)
		for _, l := range e.Limitations {
			fmt.Printf("  but %s\n", l)
		}
	}
	if n == 0 {
		rea.Log.Info("no matching references (link prints the literals that may be references)")
	}
	return nil
}

func cmdMineDocs(args []string) error {
	var source, version, docs, out string
	fs := newFlagSet("mine-docs")
	fs.StringVar(&source, "provider", "", "provider source, e.g. hashicorp/aws")
	fs.StringVar(&version, "version", "", "provider version (default: latest)")
	fs.StringVar(&docs, "docs", "", "use this local docs directory instead of fetching")
	fs.StringVar(&out, "out", "", "pack directory (default: the workspace's, .desprawl/pack/<name>, when run in one)")
	fs.Parse(args)
	name, err := rea.ProviderName(source)
	if err != nil {
		return err
	}
	if out == "" {
		if _, err := os.Stat(paths.ConfigFile(".")); err != nil {
			return errors.New("-out is required outside a workspace (one is a directory with desprawl.hcl)")
		}
		out = filepath.Join(paths.Pack("."), name)
	}
	if docs == "" {
		dir, v, err := mine.Fetch(context.Background(), source, version)
		if err != nil {
			return err
		}
		rea.Log.Info("fetched %s %s docs", source, v)
		docs = dir
	}
	rs, err := mine.Docs(docs)
	if err != nil {
		return err
	}
	imports, err := mine.Imports(docs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := rules.Save(filepath.Join(out, "rules.docs.json"), rs); err != nil {
		return err
	}
	vocab, err := mine.Vocabulary(docs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "vocabulary.docs.txt"), []byte(strings.Join(vocab, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	b, err := json.MarshalIndent(imports, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "imports.docs.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	rea.Log.Info("mined %d rules and import examples for %d types -> %s", len(rs), len(imports), out)
	return nil
}

// cmdCheckPack reports the lines of the pack files fixed from accounts
// that mention this workspace's account data.
func cmdCheckPack(args []string) error {
	dir := flags("check-pack", args, nil)
	w, err := rea.Open(*dir)
	if err != nil {
		return err
	}
	schemas, err := w.Schemas(context.Background())
	if err != nil {
		return err
	}
	leaks := w.CheckPack(schemas)
	for _, l := range leaks {
		fmt.Println(l)
	}
	n := len(leaks)
	if n > 0 {
		return fmt.Errorf("%d lines mention account data", n)
	}
	rea.Log.Info("no account data found in the pack")
	return nil
}

func split(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
