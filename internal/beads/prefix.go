package beads

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// DeriveShortPrefix generates a short beads prefix from a rig/directory name.
//
// Examples: "gastown" -> "gt", "LokustGasTown" -> "lgt", "my-project" -> "mp",
// "foo" -> "foo".
//
// It NEVER returns the raw directory name. Callers (rig registration and the
// Dolt issue_prefix seed in doltserver) rely on this so a rig is never seeded
// with a directory-name prefix like "LokustGasTown-*" (lgt-bto).
func DeriveShortPrefix(name string) string {
	// Strip path separators — callers should validate names, but be defensive
	name = filepath.Base(name)
	name = strings.TrimLeft(name, "/\\")

	// Remove common suffixes
	name = strings.TrimSuffix(name, "-py")
	name = strings.TrimSuffix(name, "-go")

	// Split on hyphens/underscores
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	})

	// If single part, try camelCase splitting first (e.g., "myProject" -> "my" + "Project"),
	// then fall back to compound word detection (e.g., "gastown" -> "gas" + "town").
	if len(parts) == 1 {
		if camelParts := splitCamelCase(parts[0]); len(camelParts) >= 2 {
			parts = camelParts
		} else {
			parts = splitCompoundWord(parts[0])
		}
	}

	if len(parts) >= 2 {
		// Take first letter of each part: "gas-town" -> "gt"
		prefix := ""
		for _, p := range parts {
			if len(p) > 0 {
				prefix += string(p[0])
			}
		}
		return strings.ToLower(prefix)
	}

	// Single word: use first 2-3 chars
	if len(name) <= 3 {
		return strings.ToLower(name)
	}
	return strings.ToLower(name[:2])
}

// splitCompoundWord attempts to split a compound word into its components.
// Common suffixes like "town", "ville", "port" are detected to split
// compound names (e.g., "gastown" -> ["gas", "town"]).
func splitCompoundWord(word string) []string {
	word = strings.ToLower(word)

	// Common suffixes for compound place names
	suffixes := []string{"town", "ville", "port", "place", "land", "field", "wood", "ford"}

	for _, suffix := range suffixes {
		if strings.HasSuffix(word, suffix) && len(word) > len(suffix) {
			prefix := word[:len(word)-len(suffix)]
			if len(prefix) > 0 {
				return []string{prefix, suffix}
			}
		}
	}

	return []string{word}
}

// splitCamelCase splits a camelCase or PascalCase string into its word parts.
// Examples: "myProject" -> ["my", "Project"], "gasStation" -> ["gas", "Station"],
// "HTMLParser" -> ["HTML", "Parser"].
func splitCamelCase(s string) []string {
	if s == "" {
		return nil
	}
	var parts []string
	start := 0
	runes := []rune(s)
	for i := 1; i < len(runes); i++ {
		// Split when transitioning from lower to upper: "myProject" at 'P'
		if unicode.IsLower(runes[i-1]) && unicode.IsUpper(runes[i]) {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
		// Split when transitioning from upper run to upper+lower: "HTMLParser" at 'P'
		if i >= 2 && unicode.IsUpper(runes[i-1]) && unicode.IsUpper(runes[i-2]) && unicode.IsLower(runes[i]) {
			parts = append(parts, string(runes[start:i-1]))
			start = i - 1
		}
	}
	parts = append(parts, string(runes[start:]))
	return parts
}

// PrefixFromConfigYAML reads the configured issue prefix from beadsDir/config.yaml.
// It honors the "issue-prefix:" key first, then "prefix:". Surrounding quotes and a
// trailing dash are stripped, and the candidate is validated against prefixRe.
// Returns "" if the file is missing or contains no valid prefix.
func PrefixFromConfigYAML(beadsDir string) string {
	configPath := filepath.Join(beadsDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		for _, key := range []string{"issue-prefix:", "prefix:"} {
			if strings.HasPrefix(line, key) {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					candidate := strings.TrimSpace(parts[1])
					// Strip quotes first, then trailing dash — matches
					// detectBeadsPrefixFromConfig in rig/manager.go.
					candidate = stripYAMLQuotes(candidate)
					candidate = strings.TrimSuffix(candidate, "-")
					if candidate != "" && prefixRe.MatchString(candidate) {
						return candidate
					}
				}
			}
		}
	}
	return ""
}
