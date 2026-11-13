package main

// Commands and flags from before desprawl stopped calling a model, kept so
// scripts and installed skills that use them are told where their work
// went and still get the nearest equivalent. They aren't in the help.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/askrike/desprawl/internal/rea"
)

func init() {
	for name, now := range map[string]string{
		"infer-rules":     "link prints the values that may be references, with the rule for each",
		"infer-list-args": "discover prints the types that need list arguments, with a spec for each",
		"infer-inventory": "discover prints the inventory kinds with no mapping and the sources to fix",
		"infer-pack":      "discover and link print what pack.json is missing (scope parameters, exclusions, dropped attributes)",
	} {
		commandsByName[name] = command{name: name, run: func(args []string) error {
			fmt.Fprintf(os.Stderr, "desprawl %s is gone: desprawl no longer calls a model; %s.\nShowing `desprawl status` instead.\n\n", name, now)
			return cmdStatus([]string{"-C", compatDir(name, args)})
		}}
	}
	commandsByName["bootstrap"] = command{name: "bootstrap", run: cmdBootstrap}
}

// cmdBootstrap does what bootstrap did that needs no model: mine the
// provider's docs if the pack has none, then discover, which prints what
// the pack is missing.
func cmdBootstrap(args []string) error {
	dir := compatDir("bootstrap", args)
	fmt.Fprintln(os.Stderr, "desprawl bootstrap is gone: desprawl no longer calls a model. Mining the docs and discovering;\nthe pack is completed from what discover and link print (see `desprawl help`).")
	w, err := rea.Open(dir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(w.Packs(), w.Provider().Name, "rules.docs.json")); errors.Is(err, os.ErrNotExist) {
		if w.Provider().Source == "" {
			return errors.New("provider source unknown; run mine-docs -provider NS/NAME")
		}
		if err := cmdMineDocs([]string{"-provider", w.Provider().Source}); err != nil {
			return err
		}
	}
	return cmdDiscover([]string{"-C", dir})
}

// compatDir parses an old command's arguments, accepting the flags it
// took, and returns the workspace.
func compatDir(name string, args []string) string {
	return *flags(name, args, func(fs *flag.FlagSet) {
		fs.String("model", "", "ignored")
		fs.String("out", "", "ignored")
		fs.String("rules", "", "ignored")
		fs.Bool("dry-run", false, "ignored")
	})
}

// noLLM registers -no-llm, which generate and run took to skip the model.
func noLLM(fs *flag.FlagSet) {
	fs.Bool("no-llm", false, "ignored: desprawl never calls a model")
}
