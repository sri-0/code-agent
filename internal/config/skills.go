package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// SkillsIndex holds the host-side view of available skill bundles. Each
// bundle is a subdirectory of Root containing a SKILL.md (and any number
// of supporting files). The bundle's directory name is its bundle name —
// what boards.yaml / stages.yaml reference in their `skills:` lists.
//
// Nil-safe: if the root dir is empty/missing, Resolve returns nothing as
// long as the caller requests no skills.
type SkillsIndex struct {
	Root    string
	bundles map[string]string // name -> absolute dir
}

// LoadSkills scans root for skill bundles. Non-bundle entries (files or
// dirs without SKILL.md) are ignored. Empty root is allowed and returns
// an empty index (callers can still function if they request no skills).
func LoadSkills(root string) (*SkillsIndex, error) {
	idx := &SkillsIndex{Root: root, bundles: map[string]string{}}
	if root == "" {
		return idx, nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	idx.Root = abs
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(abs, e.Name())
		if _, err := os.Stat(filepath.Join(full, "SKILL.md")); err != nil {
			continue
		}
		idx.bundles[e.Name()] = full
	}
	return idx, nil
}

// Resolve returns the absolute directories for the requested bundle names.
// Missing bundles produce an error rather than a silent skip — catching
// typos in boards.yaml early is more useful than shipping a stage without
// the skill it claimed to need.
func (s *SkillsIndex) Resolve(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if s == nil || len(s.bundles) == 0 {
		return nil, fmt.Errorf("skills requested (%v) but none available under %q", names, rootOrDash(s))
	}
	var out []string
	var missing []string
	for _, n := range names {
		p, ok := s.bundles[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		out = append(out, p)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unknown skills %v (available under %s: %v)", missing, s.Root, s.Available())
	}
	return out, nil
}

// Available returns the sorted-ish list of bundle names currently indexed.
// Callers use this in error messages.
func (s *SkillsIndex) Available() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.bundles))
	for name := range s.bundles {
		out = append(out, name)
	}
	return out
}

func rootOrDash(s *SkillsIndex) string {
	if s == nil || s.Root == "" {
		return "-"
	}
	return s.Root
}
