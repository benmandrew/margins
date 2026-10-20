// Package config loads the service's own config.toml, which Ansible renders,
// and the per-commit .typst-review.toml read from the thesis repository.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Build is the per-commit build configuration. The service's defaults fill any
// field a commit's .typst-review.toml leaves out, or the whole struct when the
// file is missing.
type Build struct {
	Typst       string   `toml:"typst"`
	Entrypoints []string `toml:"entrypoints"`
	Fonts       string   `toml:"fonts"`
	Branches    []string `toml:"branches"`
	ReviewTags  string   `toml:"review_tags"`
}

type Repo struct {
	// FullName is the Gitea "owner/name". A webhook naming any other
	// repository is rejected.
	FullName      string `toml:"full_name"`
	CloneURL      string `toml:"clone_url"`
	DefaultBranch string `toml:"default_branch"`
}

type Limits struct {
	BuildTimeout   time.Duration `toml:"build_timeout"`
	Workers        int           `toml:"workers"`
	MaxOutputBytes int64         `toml:"max_output_bytes"`
	DiffQueue      int           `toml:"diff_queue"`
	MaxAttempts    int           `toml:"max_attempts"`
	BackfillDepth  int           `toml:"backfill_depth"`
	DiskBudget     int64         `toml:"disk_budget_bytes"`
	NoteMaxChars   int           `toml:"note_max_chars"`
	NotesPerCommit int           `toml:"notes_per_commit"`
}

type Timers struct {
	Poll        time.Duration `toml:"poll"`
	Backfill    time.Duration `toml:"backfill"`
	Retry       time.Duration `toml:"retry"`
	ReviewDiffs time.Duration `toml:"review_diffs"`
	Prune       time.Duration `toml:"prune"`
	// SnapshotAt is a daily wall-clock time, "HH:MM" in Config.Timezone.
	SnapshotAt string `toml:"snapshot_at"`
}

type Role string

const (
	RoleAuthor     Role = "author"
	RoleSupervisor Role = "supervisor"
)

type User struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
	Role Role   `toml:"role"`
	// EnrolHash is the hex SHA-256 of the user's one-time enrolment secret.
	// The secret itself never reaches the container.
	EnrolHash string `toml:"enrol_hash"`
}

// Config is the service configuration. Secrets are absent on purpose: they
// arrive through the environment (see Secrets), so the rendered file can be
// world-readable inside the container.
type Config struct {
	Listen  string `toml:"listen"`
	DataDir string `toml:"data_dir"`
	// Origin is the public origin. The WebAuthn relying-party ID is its host,
	// and every POST must carry exactly this Origin.
	Origin string `toml:"origin"`
	// ClientIPHeader names the header nginx sets to the client address.
	ClientIPHeader string `toml:"client_ip_header"`
	Timezone       string `toml:"timezone"`
	Repo           Repo   `toml:"repo"`
	// Defaults is the build configuration for commits with no
	// .typst-review.toml, and the fallback for fields one leaves out.
	Defaults Build  `toml:"defaults"`
	Limits   Limits `toml:"limits"`
	Timers   Timers `toml:"timers"`
	// TypstHashes maps "<version>/<target>", for example
	// "0.15.1/aarch64-unknown-linux-musl", to the SHA-256 of that release
	// archive. A version with no entry is never downloaded.
	TypstHashes map[string]string `toml:"typst_hashes"`
	Users       []User            `toml:"users"`

	// Location is Timezone, loaded.
	Location *time.Location `toml:"-"`
}

// User returns the configured user with this ID.
func (c Config) User(id string) (User, bool) {
	for _, u := range c.Users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

// Secrets come from the environment, which the Ansible role fills from the
// vault through the container's .env.
type Secrets struct {
	WebhookSecret string
	GiteaToken    string
}

func defaults() Config {
	return Config{
		Listen:         ":8080",
		DataDir:        "/data",
		Origin:         "https://thesis.benmandrew.com",
		ClientIPHeader: "X-Real-IP",
		Timezone:       "Europe/London",
		Repo:           Repo{DefaultBranch: "main"},
		Defaults: Build{
			Entrypoints: []string{"main.typ"},
			Branches:    []string{"main"},
			ReviewTags:  "review-*",
		},
		Limits: Limits{
			BuildTimeout:   120 * time.Second,
			Workers:        1,
			MaxOutputBytes: 200 << 20,
			DiffQueue:      10,
			MaxAttempts:    3,
			BackfillDepth:  200,
			DiskBudget:     4 << 30,
			NoteMaxChars:   4000,
			NotesPerCommit: 500,
		},
		Timers: Timers{
			Poll:        10 * time.Minute,
			Backfill:    time.Hour,
			Retry:       time.Hour,
			ReviewDiffs: time.Hour,
			Prune:       24 * time.Hour,
			SnapshotAt:  "01:30",
		},
	}
}

// Load reads the service config from path over the built-in defaults.
func Load(path string) (Config, error) {
	cfg := defaults()
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Config{}, fmt.Errorf("config %s: unknown keys %v", path, undecoded)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: timezone: %w", path, err)
	}
	cfg.Location = loc
	return cfg, nil
}

func (c Config) validate() error {
	var errs []error
	if c.Repo.FullName == "" {
		errs = append(errs, errors.New("repo.full_name is required"))
	}
	if c.Repo.CloneURL == "" {
		errs = append(errs, errors.New("repo.clone_url is required"))
	}
	if !ValidRef(c.Repo.DefaultBranch) {
		errs = append(errs, fmt.Errorf("repo.default_branch %q is not a valid branch name", c.Repo.DefaultBranch))
	}
	if c.Defaults.Typst != "" && !TypstVersion.MatchString(c.Defaults.Typst) {
		errs = append(errs, fmt.Errorf("defaults.typst %q is not an exact version", c.Defaults.Typst))
	}
	if c.Limits.Workers < 1 {
		errs = append(errs, errors.New("limits.workers must be at least 1"))
	}
	if c.Limits.MaxAttempts < 1 {
		errs = append(errs, errors.New("limits.max_attempts must be at least 1"))
	}
	for key, hash := range c.TypstHashes {
		version, target, ok := strings.Cut(key, "/")
		if !ok || !TypstVersion.MatchString(version) || target == "" {
			errs = append(errs, fmt.Errorf("typst_hashes key %q is not <version>/<target>", key))
		}
		if !sha256Hex.MatchString(hash) {
			errs = append(errs, fmt.Errorf("typst_hashes[%q] is not a SHA-256 in hex", key))
		}
	}
	if u, err := url.Parse(c.Origin); err != nil || u.Scheme != "https" && u.Hostname() != "localhost" || u.Host == "" || u.Path != "" {
		errs = append(errs, fmt.Errorf("origin %q must be a bare https origin", c.Origin))
	}
	if _, err := time.Parse("15:04", c.Timers.SnapshotAt); err != nil {
		errs = append(errs, fmt.Errorf("timers.snapshot_at %q is not HH:MM", c.Timers.SnapshotAt))
	}
	seen := map[string]bool{}
	for _, u := range c.Users {
		if !UserID.MatchString(u.ID) {
			errs = append(errs, fmt.Errorf("user id %q does not match %s", u.ID, UserID))
		}
		if seen[u.ID] {
			errs = append(errs, fmt.Errorf("user id %q appears twice", u.ID))
		}
		seen[u.ID] = true
		if u.Role != RoleAuthor && u.Role != RoleSupervisor {
			errs = append(errs, fmt.Errorf("user %q: role %q is not author or supervisor", u.ID, u.Role))
		}
		if !sha256Hex.MatchString(u.EnrolHash) {
			errs = append(errs, fmt.Errorf("user %q: enrol_hash is not a SHA-256 in hex", u.ID))
		}
		if u.Name == "" {
			errs = append(errs, fmt.Errorf("user %q: name is empty", u.ID))
		}
	}
	if c.Limits.NoteMaxChars < 1 || c.Limits.NotesPerCommit < 1 {
		errs = append(errs, errors.New("limits.note_max_chars and limits.notes_per_commit must be at least 1"))
	}
	return errors.Join(errs...)
}

// LoadSecrets reads the secrets from the environment and fails when either is
// missing, so a bad .env stops the container at boot.
func LoadSecrets() (Secrets, error) {
	s := Secrets{
		WebhookSecret: os.Getenv("THESIS_WEBHOOK_SECRET"),
		GiteaToken:    os.Getenv("THESIS_GITEA_TOKEN"),
	}
	var errs []error
	if s.WebhookSecret == "" {
		errs = append(errs, errors.New("THESIS_WEBHOOK_SECRET is not set"))
	}
	if s.GiteaToken == "" {
		errs = append(errs, errors.New("THESIS_GITEA_TOKEN is not set"))
	}
	return s, errors.Join(errs...)
}

// ParseBuild reads a commit's .typst-review.toml and fills what it leaves out
// from base. A nil or empty data means the file is missing at that commit.
func ParseBuild(data []byte, base Build) (Build, error) {
	b := base
	if len(data) > 0 {
		// Decode into a zero value first, so a key the file sets to an empty
		// list is distinguishable from a key it leaves out.
		var file Build
		md, err := toml.Decode(string(data), &file)
		if err != nil {
			return Build{}, fmt.Errorf(".typst-review.toml: %w", err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			return Build{}, fmt.Errorf(".typst-review.toml: unknown keys %v", undecoded)
		}
		if md.IsDefined("typst") {
			b.Typst = file.Typst
		}
		if md.IsDefined("entrypoints") {
			b.Entrypoints = file.Entrypoints
		}
		if md.IsDefined("fonts") {
			b.Fonts = file.Fonts
		}
		if md.IsDefined("branches") {
			b.Branches = file.Branches
		}
		if md.IsDefined("review_tags") {
			b.ReviewTags = file.ReviewTags
		}
	}
	return b, b.validate()
}

func (b Build) validate() error {
	var errs []error
	if !TypstVersion.MatchString(b.Typst) {
		errs = append(errs, fmt.Errorf("typst %q is not an exact version", b.Typst))
	}
	if len(b.Entrypoints) == 0 {
		errs = append(errs, errors.New("entrypoints is empty"))
	}
	for _, e := range b.Entrypoints {
		if !Entrypoint.MatchString(e) {
			errs = append(errs, fmt.Errorf("entrypoint %q must be a relative .typ path with no '..'", e))
		}
	}
	if b.Fonts != "" && !RelPath.MatchString(b.Fonts) {
		errs = append(errs, fmt.Errorf("fonts %q must be a relative path with no '..'", b.Fonts))
	}
	for _, br := range b.Branches {
		if !ValidRef(br) {
			errs = append(errs, fmt.Errorf("branch %q is not a valid branch name", br))
		}
	}
	return errors.Join(errs...)
}
