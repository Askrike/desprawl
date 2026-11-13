package main

// desprawl update: replace this binary with the latest release, the way
// deploy/install/install.sh installs it, then run the new binary's setup.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/askrike/desprawl/internal/paths"
)

// installURL serves the releases (deploy/install/handler.py).
const installURL = "https://desprawl.spawned.app"

func cmdUpdate(args []string) error {
	fl := newFlagSet("update")
	want := fl.String("version", "", "install this version (e.g. v0.1.7) instead of the latest")
	noSetup := fl.Bool("no-setup", false, "don't run the new binary's setup afterwards")
	fl.Parse(args)

	exe, err := paths.Exe()
	if err != nil {
		return err
	}
	if version == "" {
		return fmt.Errorf("%s isn't a release build (version %s): update it from its checkout, or install a release with\n  curl -fsSL %s | sh", exe, versionString(), installURL)
	}

	target := *want
	if target == "" {
		b, err := get(installURL + "/latest")
		if err != nil {
			return err
		}
		target = strings.TrimSpace(string(b))
	}
	if strings.TrimPrefix(target, "v") == strings.TrimPrefix(version, "v") {
		fmt.Printf("desprawl %s is up to date\n", target)
		return nil
	}

	archive, binary := "desprawl_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz", "desprawl"
	if runtime.GOOS == "windows" {
		archive, binary = "desprawl_windows_"+runtime.GOARCH+".zip", "desprawl.exe"
	}
	fmt.Printf("Downloading desprawl %s (%s/%s)\n", target, runtime.GOOS, runtime.GOARCH)
	data, err := get(installURL + "/download/" + target + "/" + archive)
	if err != nil {
		return err
	}
	sums, err := get(installURL + "/download/" + target + "/checksums.txt")
	if err != nil {
		return err
	}
	if err := checkSum(data, sums, archive); err != nil {
		return err
	}
	bin, err := extract(data, archive, binary)
	if err != nil {
		return err
	}
	if err := replaceExe(exe, bin); err != nil {
		return err
	}
	fmt.Printf("Updated desprawl %s -> %s at %s\n", version, target, exe)

	if *noSetup {
		return nil
	}
	// The new binary's skills and MCP config, not this one's.
	fmt.Println()
	cmd := exec.Command(exe, "setup")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("desprawl setup: %w", err)
	}
	fmt.Println("\nRestart your agents to use the new MCP server.")
	return nil
}

func get(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// checkSum checks data against archive's line in a goreleaser checksums.txt.
func checkSum(data, sums []byte, archive string) error {
	got := sha256.Sum256(data)
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == archive {
			if f[0] != hex.EncodeToString(got[:]) {
				return fmt.Errorf("checksum mismatch for %s", archive)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt has no %s", archive)
}

// extract returns the file named binary from a .tar.gz or .zip archive.
func extract(data []byte, archive, binary string) ([]byte, error) {
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) == binary {
				r, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer r.Close()
				return io.ReadAll(r)
			}
		}
		return nil, fmt.Errorf("%s has no %s", archive, binary)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s has no %s", archive, binary)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == binary {
			return io.ReadAll(tr)
		}
	}
}

// replaceExe puts bin at exe with a rename, never by writing over exe: macOS
// caches a binary's code signature by file, so new bytes written into the
// old file get the process killed when it runs. A rename also leaves
// running copies (an agent's MCP server) on the old file.
func replaceExe(exe string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".desprawl-update-*")
	if err != nil {
		return fmt.Errorf("can't write next to %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" { // a running .exe can be renamed but not replaced
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return err
		}
	}
	return os.Rename(tmp.Name(), exe)
}
