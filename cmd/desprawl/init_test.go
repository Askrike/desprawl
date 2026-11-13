package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/askrike/desprawl/internal/model"
	"github.com/askrike/desprawl/internal/rea"
)

// The answers to init's questions make a desprawl.hcl that reads back the
// same: two providers (one aliased), settings, environment, what to draw.
func TestAskInit(t *testing.T) {
	answers := strings.Join([]string{
		"",                            // reverse-engineer: yes
		"hashicorp/aws",               // source
		"~> 6.0",                      // version
		`region = "eu-central-1"`, "", // settings, then the empty line ending them
		"AWS_PROFILE=dev", // env
		"y",               // another
		"hashicorp/aws",   // source
		"",                // version
		`region = "us-east-1"`, "",
		"",   // env
		"us", // alias
		"n",  // no more
		"",   // default output
		"",   // draw: the imported Terraform
	}, "\n") + "\n"
	ps, draw, output, err := askInit(strings.NewReader(answers), io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := model.ParseConfig(rea.RenderConfig(ps, draw, output), "desprawl.hcl")
	if err != nil {
		t.Fatalf("%v\n%s", err, rea.RenderConfig(ps, draw, output))
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Ref() != "aws" || cfg.Providers[1].Ref() != "aws.us" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	if p := cfg.Providers[0]; p.Version != "~> 6.0" || p.Env["AWS_PROFILE"] != "dev" || p.Body != `region = "eu-central-1"` {
		t.Errorf("first provider = %+v", p)
	}
	if len(cfg.Draw) != 1 || cfg.Draw[0] != ".desprawl/imported" || cfg.Output != ".desprawl/imported" {
		t.Errorf("draw %v, output %q", cfg.Draw, cfg.Output)
	}
	tf := string(cfg.ProvidersTF())
	if !strings.Contains(tf, `alias = "us"`) || strings.Count(tf, `source = "hashicorp/aws"`) != 1 {
		t.Errorf("providers.tf:\n%s", tf)
	}
}

// Scripts, CI and agents run commands with /dev/null as stdin: that's no
// terminal to ask questions in.
func TestDevNullIsNoTerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("isTerminal(/dev/null) = true")
	}
}

// Run again in a workspace, init starts from what desprawl.hcl says:
// Enter keeps a provider and the draw list, and new answers add to them.
func TestAskInitAgain(t *testing.T) {
	cur, err := model.ParseConfig([]byte(`provider "aws" {
  source = "hashicorp/aws"
  env    = { AWS_PROFILE = "dev" }
  config {
    region = "eu-central-1"
  }
}
draw = ["../infra"]
`), "desprawl.hcl")
	if err != nil {
		t.Fatal(err)
	}
	answers := strings.Join([]string{
		"",                      // keep aws
		"y",                     // add one
		"cloudflare/cloudflare", // source
		"", "",                  // version, no settings
		"",  // env
		"n", // no more
		"",  // keep output
		"",  // keep draw
	}, "\n") + "\n"
	ps, draw, output, err := askInit(strings.NewReader(answers), io.Discard, &cur)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := model.ParseConfig(rea.RenderConfig(ps, draw, output), "desprawl.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[0].Body != `region = "eu-central-1"` || cfg.Providers[0].Env["AWS_PROFILE"] != "dev" || cfg.Providers[1].Name != "cloudflare" {
		t.Errorf("providers = %+v", cfg.Providers)
	}
	if len(cfg.Draw) != 1 || cfg.Draw[0] != "../infra" {
		t.Errorf("draw = %v", cfg.Draw)
	}
}
