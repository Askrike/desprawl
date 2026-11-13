package mine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var identifier = regexp.MustCompile(`[A-Za-z0-9_]+`)

// Vocabulary returns every word in the provider's documentation, lower
// case: identifiers as written ("clustername", "aws_iam_role") and their
// alphanumeric parts. It is what the provider makes public; account data
// in pack files is whatever isn't in it.
func Vocabulary(dir string) ([]string, error) {
	words := map[string]bool{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMarkdown(d.Name()) {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, w := range identifier.FindAllString(string(b), -1) {
			w = strings.ToLower(w)
			if len(w) >= 4 {
				words[w] = true
			}
			for _, p := range strings.Split(w, "_") {
				if len(p) >= 4 {
					words[p] = true
				}
			}
		}
		return nil
	})
	out := make([]string, 0, len(words))
	for w := range words {
		out = append(out, w)
	}
	sort.Strings(out)
	return out, err
}
