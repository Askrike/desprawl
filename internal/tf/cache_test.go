package tf

import (
	"github.com/askrike/desprawl/internal/logs"
	"os"
	"path/filepath"
	"testing"
)

const downloadedLock = `# This file is maintained automatically by "terraform init".
# Manual edits may be lost in future updates.

provider "registry.terraform.io/hashicorp/aws" {
  version     = "6.71.0"
  constraints = "~> 6.0"
  hashes = [
    "h1:local",
    "zh:aaa",
    "zh:bbb",
  ]
}
`

func TestKeepLockHashes(t *testing.T) {
	cache := t.TempDir()
	var logged []string
	run := func(lock string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, lockFile), []byte(lock), 0o644); err != nil {
			t.Fatal(err)
		}
		r := Runner{Dir: dir, ProviderCache: cache, Log: func(_ logs.Level, s string) { logged = append(logged, s) }}
		r.keepLockHashes()
		b, err := os.ReadFile(filepath.Join(dir, lockFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Installed from the cache before any download recorded its checksums.
	fromCache := `provider "registry.terraform.io/hashicorp/aws" {
  version     = "6.71.0"
  constraints = ">= 6.1"
  hashes = [
    "h1:local",
  ]
}
`
	if got := run(fromCache); got != fromCache || len(logged) != 1 {
		t.Fatalf("unknown version: lock changed or not reported:\n%s\n%q", got, logged)
	}

	if got := run(downloadedLock); got != downloadedLock {
		t.Fatalf("a downloaded lock changed:\n%s", got)
	}
	want := `provider "registry.terraform.io/hashicorp/aws" {
  version     = "6.71.0"
  constraints = ">= 6.1"
  hashes = [
    "h1:local",
    "zh:aaa",
    "zh:bbb",
  ]
}
`
	if got := run(fromCache); got != want {
		t.Fatalf("checksums not restored:\ngot\n%s\nwant\n%s", got, want)
	}

	// A package that isn't the one downloaded keeps its own checksum.
	other := `provider "registry.terraform.io/hashicorp/aws" {
  version = "6.71.0"
  hashes  = ["h1:other"]
}
`
	logged = nil
	if got := run(other); got != other || len(logged) != 1 {
		t.Fatalf("mismatched checksum: lock changed or not reported:\n%s\n%q", got, logged)
	}
}
