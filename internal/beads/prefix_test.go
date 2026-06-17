package beads

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveShortPrefix(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		// Compound words with common suffixes should split
		{"gastown", "gt"},     // gas + town
		{"nashville", "nv"},   // nash + ville
		{"bridgeport", "bp"},  // bridge + port
		{"someplace", "sp"},   // some + place
		{"greenland", "gl"},   // green + land
		{"springfield", "sf"}, // spring + field
		{"hollywood", "hw"},   // holly + wood
		{"oxford", "of"},      // ox + ford

		// camelCase / PascalCase names — the regression case (lgt-bto):
		// "LokustGasTown" must derive "lgt", never the raw directory name.
		{"LokustGasTown", "lgt"},
		{"GasTownTower", "gtt"},
		{"AutoIndex", "ai"},
		{"myProject", "mp"},
		{"gasStation", "gs"},
		{"HTMLParser", "hp"},

		// Hyphenated names
		{"my-project", "mp"},
		{"gas-town", "gt"},
		{"some-long-name", "sln"},

		// Underscored names
		{"my_project", "mp"},

		// Short single words (use the whole name)
		{"foo", "foo"},
		{"bar", "bar"},
		{"ab", "ab"},

		// Longer single words without known suffixes (first 2 chars)
		{"myrig", "my"},
		{"awesome", "aw"},
		{"coolrig", "co"},

		// With language suffixes stripped
		{"myproject-py", "my"},
		{"myproject-go", "my"},

		// Path-like names (slashes stripped)
		{"/my_app", "ma"},
		{"/some/deep/path", "pa"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveShortPrefix(tt.name)
			if got != tt.want {
				t.Errorf("DeriveShortPrefix(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestSplitCompoundWord(t *testing.T) {
	tests := []struct {
		word string
		want []string
	}{
		// Known suffixes
		{"gastown", []string{"gas", "town"}},
		{"nashville", []string{"nash", "ville"}},
		{"bridgeport", []string{"bridge", "port"}},
		{"someplace", []string{"some", "place"}},
		{"greenland", []string{"green", "land"}},
		{"springfield", []string{"spring", "field"}},
		{"hollywood", []string{"holly", "wood"}},
		{"oxford", []string{"ox", "ford"}},

		// Just the suffix (should not split)
		{"town", []string{"town"}},
		{"ville", []string{"ville"}},

		// No known suffix
		{"myrig", []string{"myrig"}},
		{"awesome", []string{"awesome"}},
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := splitCompoundWord(tt.word)
			if len(got) != len(tt.want) {
				t.Errorf("splitCompoundWord(%q) = %v, want %v", tt.word, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCompoundWord(%q)[%d] = %q, want %q", tt.word, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestSplitCamelCase(t *testing.T) {
	tests := []struct {
		word string
		want []string
	}{
		// Basic camelCase
		{"myProject", []string{"my", "Project"}},
		{"gasStation", []string{"gas", "Station"}},

		// PascalCase
		{"MyProject", []string{"My", "Project"}},

		// Uppercase runs
		{"HTMLParser", []string{"HTML", "Parser"}},
		{"parseJSON", []string{"parse", "JSON"}},

		// No splits (single word, all lower)
		{"gastown", []string{"gastown"}},
		{"a", []string{"a"}},

		// All uppercase (no lower transition)
		{"AB", []string{"AB"}},

		// Empty
		{"", nil},
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			got := splitCamelCase(tt.word)
			if len(got) != len(tt.want) {
				t.Errorf("splitCamelCase(%q) = %v, want %v", tt.word, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCamelCase(%q)[%d] = %q, want %q", tt.word, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestPrefixFromConfigYAML(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if got := PrefixFromConfigYAML(t.TempDir()); got != "" {
			t.Errorf("PrefixFromConfigYAML(missing) = %q, want \"\"", got)
		}
	})

	t.Run("issue-prefix wins and trailing dash stripped", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "issue-prefix: lgt-\nprefix: ignored\n")
		if got := PrefixFromConfigYAML(dir); got != "lgt" {
			t.Errorf("PrefixFromConfigYAML() = %q, want lgt", got)
		}
	})

	t.Run("prefix key honored", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "prefix: \"ai\"\n")
		if got := PrefixFromConfigYAML(dir); got != "ai" {
			t.Errorf("PrefixFromConfigYAML() = %q, want ai", got)
		}
	})

	t.Run("invalid prefix rejected", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigYAML(t, dir, "issue-prefix: 1bad!\n")
		if got := PrefixFromConfigYAML(dir); got != "" {
			t.Errorf("PrefixFromConfigYAML(invalid) = %q, want \"\"", got)
		}
	})
}

func writeConfigYAML(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
