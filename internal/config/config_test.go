package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
[repo]
full_name = "me/thesis"
clone_url = "http://100.64.0.1:3000/me/thesis.git"
`

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.Workers != 1 || cfg.Timers.Poll != 10*time.Minute || cfg.Limits.DiskBudget != 4<<30 {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestLoadOverridesDurations(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimal+"[timers]\npoll = \"1h\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timers.Poll != time.Hour {
		t.Errorf("poll = %v, want 1h", cfg.Timers.Poll)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, body := range map[string]string{
		"missing repo":  `listen = ":8080"`,
		"unknown key":   minimal + "colour = \"blue\"\n",
		"bad hash":      minimal + "[typst_hashes]\n\"0.15.1/x86_64-unknown-linux-musl\" = \"abc\"\n",
		"bad hash key":  minimal + "[typst_hashes]\n\"latest\" = \"" + strings.Repeat("a", 64) + "\"\n",
		"option branch": minimal[:len(minimal)-1] + "\ndefault_branch = \"--upload-pack=x\"\n",
		"zero workers":  minimal + "[limits]\nworkers = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Error("Load succeeded, want an error")
			}
		})
	}
}

var base = Build{Typst: "0.15.1", Entrypoints: []string{"main.typ"}, Branches: []string{"main"}, ReviewTags: "review-*"}

func TestParseBuildMissingFileUsesBase(t *testing.T) {
	b, err := ParseBuild(nil, base)
	if err != nil {
		t.Fatal(err)
	}
	if b.Typst != "0.15.1" || b.Entrypoints[0] != "main.typ" {
		t.Errorf("got %+v", b)
	}
}

func TestParseBuildOverridesOnlyWhatItSets(t *testing.T) {
	b, err := ParseBuild([]byte(`typst = "0.14.0"`), base)
	if err != nil {
		t.Fatal(err)
	}
	if b.Typst != "0.14.0" || b.ReviewTags != "review-*" {
		t.Errorf("got %+v", b)
	}
}

func TestParseBuildRejectsHostileValues(t *testing.T) {
	for name, body := range map[string]string{
		"traversal entry": `entrypoints = ["../etc/passwd.typ"]`,
		"absolute entry":  `entrypoints = ["/main.typ"]`,
		"option entry":    `entrypoints = ["--root=/.typ"]`,
		"non-typ entry":   `entrypoints = ["main.sh"]`,
		"empty entries":   `entrypoints = []`,
		"traversal fonts": `fonts = "../fonts"`,
		"moving version":  `typst = "latest"`,
		"option branch":   `branches = ["--upload-pack=x"]`,
		"dotdot branch":   `branches = ["a..b"]`,
		"unknown key":     `make = "all"`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBuild([]byte(body), base); err == nil {
				t.Error("ParseBuild succeeded, want an error")
			}
		})
	}
}

func TestValidRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"main":              true,
		"review-2026-10-20": true,
		"feature/chapter-3": true,
		"-x":                false,
		"a/../b":            false,
		"a..b":              false,
		"a.lock":            false,
		"a/":                false,
		"":                  false,
	} {
		if got := ValidRef(ref); got != want {
			t.Errorf("ValidRef(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../config.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := cfg.User("alice"); !ok || u.Role != RoleSupervisor {
		t.Errorf("alice = %+v, %v", u, ok)
	}
	if cfg.Location == nil || cfg.Location.String() != "Europe/London" {
		t.Errorf("location = %v", cfg.Location)
	}
}

func TestLoadRejectsBadUsers(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for name, users := range map[string]string{
		"bad id":    `[[users]]` + "\nid = \"Ben\"\nname = \"B\"\nrole = \"author\"\nenrol_hash = \"" + hash + "\"\n",
		"bad role":  `[[users]]` + "\nid = \"ben\"\nname = \"B\"\nrole = \"admin\"\nenrol_hash = \"" + hash + "\"\n",
		"bad hash":  `[[users]]` + "\nid = \"ben\"\nname = \"B\"\nrole = \"author\"\nenrol_hash = \"secret\"\n",
		"duplicate": strings.Repeat(`[[users]]`+"\nid = \"ben\"\nname = \"B\"\nrole = \"author\"\nenrol_hash = \""+hash+"\"\n", 2),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, minimal+users)); err == nil {
				t.Error("Load succeeded, want an error")
			}
		})
	}
}
