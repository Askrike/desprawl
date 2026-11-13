// desprawl draws Terraform as an architecture map (internal/tfmap) and
// reverse-engineers live infrastructure into Terraform to draw
// (internal/rea). This package is the shell around them: the command line
// (main.go, maps.go, rea.go), the map server and its page (serve.go,
// index.html), the MCP server for agents (mcp.go), and setup and update.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"

	"github.com/askrike/desprawl/internal/logs"
	"github.com/askrike/desprawl/internal/prof"
	"github.com/askrike/desprawl/internal/rea"
)

type command struct {
	name, args, help string
	run              func([]string) error
}

type section struct {
	title string
	cmds  []command
}

// sections are the commands, as help lists them. They're set in init:
// the commands' flag parsing prints help, which lists sections.
var sections []section

func init() {
	sections = []section{
		{"Draw Terraform as a map (reads .tf files; no terraform, credentials or LLM needed)", []command{
			{"serve", "", "open the workspace's page: its overview, and the interactive maps of its configurations (the default command)", cmdServe},
			{"html", "[-o FILE]", "save the map of the workspace's configurations, layouts included, as one HTML file that opens without a server", cmdHTML},
			{"ls", "", "list the workspace's Terraform configurations (what the map commands take as config)", cmdList},
			{"map", "[config] [-json]", "print a configuration's map: components, connections, the layout drawn as text, metrics", cmdMap},
			{"diff", "[config] [-base REV] [-json] [-open]", "what changed in a configuration since a git revision (default HEAD), as components and connections", cmdDiff},
			{"layout", "seed|tidy [config]", "write a starting layout (seed) or reduce its crossings (tidy), and print it", cmdLayout},
			{"unclassified", "[config]", "list resource types no catalog covers, with the role desprawl guessed", cmdUnclassified},
		}},
		{"Reverse-engineer live infrastructure into linked Terraform (needs terraform >= 1.14 and cloud credentials; read-only)", []command{
			{"init", "[-provider NS/NAME] [-version V] [-config HCL] [-env K=V,...] [-draw DIR,...] [-output DIR]", "make the current directory a workspace: write desprawl.hcl (asks when run in a terminal)", cmdInit},
			{"discover", "[-types T,...] [-exclude-types T,...] [-json]", "list live resources with terraform query; prints what it couldn't list, with fixes", cmdDiscover},
			{"add-target", "-type T (-id ID | -identity JSON) [-name N]", "add a resource discovery couldn't list", cmdAddTarget},
			{"import-format", "-type T", "show the import IDs/identities a type takes, for add-target", cmdImportFormat},
			{"generate", "[-all] [-json]", "generate config and import values (plan only); prints the resources it left out, with fixes. A rerun redoes only those and resources whose override changed", cmdGenerate},
			{"link", "[-rules R,...] [-no-parametrize] [-json]", "turn literal IDs into references -> main.tf, data.tf; prints values that may be references, with fixes", cmdLink},
			{"verify", "[-json]", "plan the linked config; prints resources whose config differs from the cloud, with fixes", cmdVerify},
			{"run", "[-json]", "discover, generate, link, verify; prints every stage's problems, with fixes", cmdRun},
			{"status", "[-json]", "summarize the workspace and print every stage's problems, with fixes", cmdStatus},
			{"why", "[-from ADDR] [-to ADDR] [-confidence C]", "explain references: why each was made and what it doesn't establish", cmdWhy},
		}},
		{"Provider packs", []command{
			{"mine-docs", "-provider NS/NAME [-version V] [-out DIR]", "build rules and import examples from provider docs", cmdMineDocs},
			{"check-pack", "", "report account data in pack files fixed from real accounts (run before sharing a pack)", cmdCheckPack},
		}},
		{"Agents", []command{
			{"setup", "[-print] [-skills DIR]", "add desprawl's MCP server and skills to every agent harness found (-skills DIR: only the skills, into DIR)", cmdSetup},
			{"mcp", "", "serve desprawl's tools to an agent over MCP (stdio)", cmdMCP},
			{"update", "[-version V] [-no-setup]", "replace this binary with the latest release, then run its setup", cmdUpdate},
			{"version", "", "print the version", cmdVersion},
		}},
	}
	for _, s := range sections {
		for _, c := range s.cmds {
			commandsByName[c.name] = c
		}
	}
}

const intro = `desprawl draws Terraform as an architecture map, and reverse-engineers live
infrastructure into Terraform to draw.

Everything works on the workspace in the current directory: one with a
desprawl.hcl (desprawl init makes it), which says what to draw and which
cloud accounts to reverse-engineer.

usage: desprawl                      serve the workspace's map
       desprawl <command> [args]     flags may come before or after the argument
       desprawl -C DIR <command>     run as if from DIR
       desprawl help <command>       a command's flags

Results go to stdout, everything else to stderr: a progress line on a
terminal (-no-progress hides it), and what a command did (-verbose adds
what each stage is doing, -debug every resource's details).
`

var commandsByName = map[string]command{}

func usage(w io.Writer) {
	fmt.Fprint(w, intro)
	for _, s := range sections {
		fmt.Fprintf(w, "\n%s:\n", s.title)
		for _, c := range s.cmds {
			fmt.Fprintf(w, "  %-16s %s\n", c.name, c.help)
		}
	}
	fmt.Fprint(w, "\nFiles: everything desprawl writes for a workspace is in its .desprawl/ (state/ is\n"+
		"git-ignored; layouts/, pack/ and overrides.json are meant to be committed). See README.md.\n")
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("desprawl: ")
	args := os.Args[1:]
	// -C DIR before the command, as with git: run as if from DIR.
	if len(args) >= 2 && args[0] == "-C" {
		if err := os.Chdir(args[1]); err != nil {
			log.Fatal(err)
		}
		args = args[2:]
	}
	name := "serve"
	if len(args) > 0 {
		switch a := args[0]; {
		case a == "help" || a == "-h" || a == "-help" || a == "--help":
			if len(args) > 1 {
				if c, ok := commandsByName[args[1]]; ok {
					if c.args == "" { // no flags: running it with -h would run it
						fmt.Printf("usage: desprawl %s\n\n%s\n", c.name, c.help)
						return
					}
					c.run([]string{"-h"})
				}
				log.Fatalf("no command %q; desprawl help lists them", args[1])
			}
			usage(os.Stdout)
			return
		case commandsByName[a].run != nil:
			name, args = a, args[1:]
		case !strings.HasPrefix(a, "-") && !isDir(a):
			log.Fatalf("%q is neither a command nor a directory; desprawl help lists the commands", a)
		}
	}
	end := prof.Span("command", "name", name)
	err := commandsByName[name].run(args)
	end(prof.Err(err)...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// newFlagSet is a command's flag set; -h prints the command's help.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		if name == "serve" {
			usage(os.Stderr)
			fmt.Fprint(os.Stderr, "\nserve flags:\n")
		} else {
			c := commandsByName[name]
			fmt.Fprintf(os.Stderr, "usage: desprawl %s %s\n\n%s\n\nflags:\n", name, c.args, c.help)
		}
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs parses a command's flags, which may come before or after its
// one argument: a directory, also accepted as -dir. With def == "" the
// directory is required.
func parseArgs(name string, args []string, reg func(*flag.FlagSet), def string) string {
	fs := newFlagSet(name)
	chdir := fs.String("C", "", "run as if from this directory: the workspace's, holding desprawl.hcl")
	dir := new(string)
	if reg != nil {
		reg(fs)
	}
	var rest []string
	for {
		fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		rest, args = append(rest, fs.Arg(0)), fs.Args()[1:]
	}
	if *chdir != "" {
		if err := os.Chdir(*chdir); err != nil {
			fmt.Fprintln(os.Stderr, "desprawl:", err)
			os.Exit(2)
		}
	}
	if len(rest) > 0 {
		*dir, rest = rest[0], rest[1:]
	}
	switch {
	case len(rest) > 0:
		fmt.Fprintf(os.Stderr, "desprawl %s: unexpected arguments %q\n\n", name, rest)
	case *dir == "" && def == "":
		fmt.Fprintf(os.Stderr, "desprawl %s: the directory is required\n\n", name)
	default:
		if *dir == "" {
			*dir = def
		}
		return *dir
	}
	fs.Usage()
	os.Exit(2)
	return ""
}

// flags parses a workspace command's flags. Commands work on the workspace
// in the current directory; -C runs one as if from another.
func flags(name string, args []string, reg func(*flag.FlagSet)) *string {
	fs := newFlagSet(name)
	dir := fs.String("C", ".", "run as if from this directory: the workspace's, holding desprawl.hcl")
	level := func(l logs.Level) func(string) error {
		return func(string) error { rea.Output.Level = max(rea.Output.Level, l); return nil }
	}
	fs.BoolFunc("verbose", "also say what each stage is doing and why it takes time (and print progress lines when stderr isn't a terminal)", level(logs.Verbose))
	fs.BoolFunc("v", "short for -verbose", level(logs.Verbose))
	fs.BoolFunc("debug", "also print every resource's and retry's details", level(logs.Debug))
	fs.BoolVar(&rea.Output.NoProgress, "no-progress", false, "don't show progress (it only shows on a terminal anyway)")
	if reg != nil {
		reg(fs)
	}
	fs.Parse(args)
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "desprawl %s works on the workspace in the current directory (-C DIR for another), so it takes no %q\n\n", name, fs.Args())
		fs.Usage()
		os.Exit(2)
	}
	return dir
}

// version is set at release time (-ldflags "-X main.version=…"); go install
// builds report their module version instead.
var version = ""

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

func cmdVersion([]string) error {
	fmt.Println(versionString())
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
