// Package skills embeds the skills `desprawl setup` installs into agent
// harnesses (SKILL.md, the Agent Skills format).
package skills

import (
	"embed"
	"strings"
)

//go:embed */*.md
var FS embed.FS

// Body returns a skill's SKILL.md without its frontmatter.
func Body(name string) (string, error) {
	b, err := FS.ReadFile(name + "/SKILL.md")
	if err != nil {
		return "", err
	}
	s := string(b)
	if rest, ok := strings.CutPrefix(s, "---\n"); ok {
		if _, body, ok := strings.Cut(rest, "\n---\n"); ok {
			s = body
		}
	}
	return strings.TrimSpace(s), nil
}
