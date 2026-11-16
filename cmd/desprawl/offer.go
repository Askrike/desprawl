package main

// Before a command in a terminal, desprawl offers to update when a newer
// release is out, the way oh-my-zsh does: at most one check a day, and one
// question per new release, which no answer brings back.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/version"
)

// noOffer are the commands desprawl never asks before: those agents run,
// and those about versions.
var noOffer = map[string]bool{"mcp": true, "update": true, "version": true, "setup": true}

type updateCheck struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest,omitempty"`
	Offered string    `json:"offered,omitempty"` // the release last asked about
}

// offerUpdate asks whether to update before command runs. On yes it
// updates, then runs the command again with the new binary and exits with
// its status.
func offerUpdate(command string) {
	cur := version.Release()
	if cur == "" || noOffer[command] || os.Getenv("DESPRAWL_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" ||
		!isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		return
	}
	file, err := paths.UpdateCheck()
	if err != nil {
		return
	}
	var c updateCheck
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &c)
	}
	now := time.Now()
	if now.Sub(c.Checked) >= 24*time.Hour {
		// Offline or slow: try again tomorrow rather than slow down every command.
		c.Checked = now
		if l, err := latest(&http.Client{Timeout: 2 * time.Second}); err == nil {
			c.Latest = l
		}
		saveUpdateCheck(file, c)
	}
	if !version.Newer(c.Latest, cur) || c.Latest == c.Offered {
		return
	}
	c.Offered = c.Latest
	saveUpdateCheck(file, c)
	fmt.Fprintf(os.Stderr, "desprawl %s is out (this is %s). Update now? [Y/n] ", c.Latest, cur)
	if !yes(os.Stdin) {
		fmt.Fprintln(os.Stderr, "Run `desprawl update` when you want it.")
		return
	}
	exe, err := update(c.Latest, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		fmt.Fprintln(os.Stderr, "Continuing with", cur)
		return
	}
	if exe == "" {
		return
	}
	fmt.Println()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = startDir // its -C is relative to it
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "DESPRAWL_NO_UPDATE_CHECK=1")
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func saveUpdateCheck(file string, c updateCheck) {
	b, _ := json.Marshal(c)
	if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
		os.WriteFile(file, b, 0o644)
	}
}

// yes reads an answer to a [Y/n] question: Enter or anything but n or no
// is yes, end of input is no. It
// reads a byte at a time, so the command gets the rest of stdin.
func yes(r io.Reader) bool {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 0 || err != nil {
			return false
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	a := strings.ToLower(strings.TrimSpace(string(line)))
	return a != "n" && a != "no"
}
