package main

// desprawl update: replace this binary with the latest release, the way
// install.sh installs it, print what's new, then run the new
// binary's setup. offerUpdate asks before a command, once per new release.

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/askrike/desprawl/internal/paths"
	"github.com/askrike/desprawl/internal/version"
)

// repoURL is where desprawl is released: each release has the archives
// .goreleaser.yaml names, checksums.txt and install.sh.
const repoURL = "https://github.com/askrike/desprawl"

// installCommand installs the latest release (install.desprawl.org
// redirects to its install.sh).
const installCommand = "curl -fsSL https://install.desprawl.org | sh"

// releasesURL lists the releases with their notes.
const releasesURL = "https://api.github.com/repos/askrike/desprawl/releases?per_page=100"

func cmdUpdate(args []string) error {
	fl := newFlagSet("update")
	want := fl.String("version", "", "install this version (e.g. v0.1.7) instead of the latest")
	noSetup := fl.Bool("no-setup", false, "don't run the new binary's setup afterwards")
	fl.Parse(args)

	target := *want
	if target == "" {
		var err error
		if target, err = latest(http.DefaultClient); err != nil {
			return err
		}
	}
	_, err := update(target, !*noSetup)
	return err
}

// latest is the newest release: where releases/latest redirects (a page,
// not the API, so it has no rate limit).
func latest(c *http.Client) (string, error) {
	nc := *c
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := nc.Get(repoURL + "/releases/latest")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	_, tag, ok := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("no release found at %s/releases (%s)", repoURL, resp.Status)
	}
	return tag, nil
}

// update replaces this binary with release target, prints the release notes
// of the versions it skips to, and runs the new binary's setup. It returns
// the binary's path, and "" when there was nothing to do.
func update(target string, setup bool) (string, error) {
	exe, err := paths.Exe()
	if err != nil {
		return "", err
	}
	cur := version.Release()
	if cur == "" {
		return "", fmt.Errorf("%s isn't a release build (version %s): update it from its checkout, or install a release with\n  %s", exe, version.String(), installCommand)
	}
	if strings.TrimPrefix(target, "v") == strings.TrimPrefix(cur, "v") {
		fmt.Printf("desprawl %s is up to date\n", cur)
		return "", nil
	}

	archive, binary := "desprawl_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz", "desprawl"
	if runtime.GOOS == "windows" {
		archive, binary = "desprawl_windows_"+runtime.GOARCH+".zip", "desprawl.exe"
	}
	fmt.Printf("Downloading desprawl %s (%s/%s)\n", target, runtime.GOOS, runtime.GOARCH)
	data, err := get(http.DefaultClient, repoURL+"/releases/download/"+target+"/"+archive)
	if err != nil {
		return "", err
	}
	sums, err := get(http.DefaultClient, repoURL+"/releases/download/"+target+"/checksums.txt")
	if err != nil {
		return "", err
	}
	if err := checkSum(data, sums, archive); err != nil {
		return "", err
	}
	bin, err := extract(data, archive, binary)
	if err != nil {
		return "", err
	}
	if err := replaceExe(exe, bin); err != nil {
		return "", err
	}
	fmt.Printf("Updated desprawl %s -> %s at %s\n", cur, target, exe)
	printNotes(os.Stdout, cur, target)

	if !setup {
		return exe, nil
	}
	// The new binary's skills and MCP config, not this one's.
	fmt.Println()
	cmd := exec.Command(exe, "setup")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("desprawl setup: %w", err)
	}
	fmt.Println("\nRestart your agents to use the new MCP server.")
	return exe, nil
}

type release struct {
	Tag   string `json:"tag_name"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
}

// printNotes prints the release notes of the published versions after from,
// up to to, oldest first. Going back prints none.
func printNotes(w io.Writer, from, to string) {
	var rs []release
	b, err := get(&http.Client{Timeout: 10 * time.Second}, releasesURL)
	if err == nil {
		err = json.Unmarshal(b, &rs)
	}
	if err != nil {
		fmt.Fprintf(w, "\nWhat's new: https://github.com/askrike/desprawl/releases (couldn't fetch the notes: %v)\n", err)
		return
	}
	rs = notesBetween(rs, from, to)
	for _, r := range rs {
		fmt.Fprintf(w, "\n## desprawl %s\n\n%s\n", r.Tag, notesText(r.Body))
	}
}

// notesBetween are the releases after from up to to, oldest first.
func notesBetween(rs []release, from, to string) []release {
	var out []release
	for _, r := range rs {
		if !r.Draft && version.Newer(r.Tag, from) && !version.Newer(r.Tag, to) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b release) int {
		if version.Newer(a.Tag, b.Tag) {
			return 1
		}
		return -1
	})
	return out
}

// notesText is a release's notes without the commit's trailers
// (Co-authored-by: …).
func notesText(body string) string {
	var keep []string
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(l, "Co-authored-by:") {
			keep = append(keep, l)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

func get(c *http.Client, url string) ([]byte, error) {
	resp, err := c.Get(url)
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
