// Package typst fetches pinned Typst release binaries and runs compiles under
// the service's limits.
package typst

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"

	"git.benmandrew.com/me/thesis/internal/config"
)

// DefaultBaseURL is where official release archives live.
const DefaultBaseURL = "https://github.com/typst/typst/releases/download"

// maxArchive caps a release download. A Typst archive is tens of megabytes.
const maxArchive = 256 << 20

// HostTarget is the release target for this machine.
func HostTarget() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64-unknown-linux-musl"
	default:
		return "x86_64-unknown-linux-musl"
	}
}

var ErrNoHash = errors.New("no pinned SHA-256 for this Typst version")

// Manager keeps one binary per version under Dir/<version>/typst. A binary
// already in the cache was hash-checked when it was downloaded.
type Manager struct {
	Dir     string
	BaseURL string
	Target  string
	Hashes  map[string]string // "<version>/<target>" -> hex SHA-256
	Client  *http.Client

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (m *Manager) lock(version string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	if m.locks[version] == nil {
		m.locks[version] = &sync.Mutex{}
	}
	return m.locks[version]
}

// Binary returns the path of the Typst binary for version, downloading it
// first if needed.
func (m *Manager) Binary(ctx context.Context, version string) (string, error) {
	if !config.TypstVersion.MatchString(version) {
		return "", fmt.Errorf("typst version %q", version)
	}
	path := filepath.Join(m.Dir, version, "typst")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	l := m.lock(version)
	l.Lock()
	defer l.Unlock()
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	want, ok := m.Hashes[version+"/"+m.Target]
	if !ok {
		return "", fmt.Errorf("typst %s for %s: %w", version, m.Target, ErrNoHash)
	}
	if err := m.download(ctx, version, want, path); err != nil {
		return "", fmt.Errorf("typst %s: %w", version, err)
	}
	return path, nil
}

func (m *Manager) download(ctx context.Context, version, wantHex, dest string) error {
	base := m.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	url := fmt.Sprintf("%s/v%s/typst-%s.tar.xz", base, version, m.Target)
	client := m.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	// Read it all and check the hash before decompressing a single byte.
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return err
	}
	if len(archive) > maxArchive {
		return fmt.Errorf("archive larger than %d bytes", maxArchive)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != wantHex {
		return fmt.Errorf("SHA-256 mismatch: got %s, pinned %s", got, wantHex)
	}
	bin, err := extractTypst(archive)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// extractTypst returns the `typst` executable from a release tar.xz. The
// archive holds one directory with the binary, a licence and a readme.
func extractTypst(archive []byte) ([]byte, error) {
	xr, err := xz.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(xr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archive has no typst binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "typst" {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// Compile options for one typst compile.
type Compile struct {
	Binary       string
	Root         string // --root; the compile cannot read outside it
	Entry        string // relative to Root
	Out          string
	FontPaths    []string
	Timestamp    int64 // --creation-timestamp, for byte-identical rebuilds
	PackageCache string
	Home         string
	Timeout      time.Duration
	MaxOutput    int64
}

// Result of a compile. Err is set only for failures of the service itself,
// never for a document that fails to compile.
type Result struct {
	Status   string // "ok", "failed" or "timeout"
	Log      []byte
	Duration time.Duration
	Size     int64
}

// maxLog keeps a runaway diagnostic stream from filling memory.
const maxLog = 1 << 20

func (c Compile) Run(ctx context.Context) (Result, error) {
	args := []string{
		"compile",
		"--root", c.Root,
		"--creation-timestamp", strconv.FormatInt(c.Timestamp, 10),
		"--package-cache-path", c.PackageCache,
		// Only the fonts Typst embeds and the repository's own, so a build
		// does not depend on what the host has installed.
		"--ignore-system-fonts",
		"--diagnostic-format", "human",
	}
	for _, fp := range c.FontPaths {
		args = append(args, "--font-path", fp)
	}
	args = append(args, "--", filepath.Join(c.Root, c.Entry), c.Out)
	return run(ctx, c.Timeout, c.MaxOutput, c.Out, c.Home, c.Binary, args...)
}

// run executes a tool under a timeout with a clean environment and collects
// its stderr. If out is set, a missing or oversized file is a failure.
func run(ctx context.Context, timeout time.Duration, maxOutput int64, out, home, name string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// No SOURCE_DATE_EPOCH or anything else from the service's own
	// environment: --creation-timestamp is the only date source.
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LC_ALL=C.UTF-8", "NO_COLOR=1"}
	cmd.WaitDelay = 5 * time.Second
	var log limitedBuffer
	log.max = maxLog
	cmd.Stdout, cmd.Stderr = &log, &log
	start := time.Now()
	err := cmd.Run()
	res := Result{Log: log.Bytes(), Duration: time.Since(start)}
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		res.Status = "timeout"
		res.Log = append(res.Log, fmt.Sprintf("\n[thesis] stopped after %s\n", timeout)...)
		return res, nil
	case err != nil:
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return res, fmt.Errorf("running %s: %w", filepath.Base(name), err)
		}
		res.Status = "failed"
		return res, nil
	}
	if out != "" {
		fi, err := os.Stat(out)
		if err != nil {
			res.Status = "failed"
			res.Log = append(res.Log, "\n[thesis] no output file was written\n"...)
			return res, nil
		}
		res.Size = fi.Size()
		if maxOutput > 0 && fi.Size() > maxOutput {
			os.Remove(out)
			res.Status = "failed"
			res.Log = append(res.Log, fmt.Sprintf("\n[thesis] output of %d bytes exceeds the %d-byte cap\n", fi.Size(), maxOutput)...)
			return res, nil
		}
	}
	res.Status = "ok"
	return res, nil
}

// Tool runs one of the diff tools under the same limits as a compile.
func Tool(ctx context.Context, timeout time.Duration, maxOutput int64, out, home, name string, args ...string) (Result, error) {
	return run(ctx, timeout, maxOutput, out, home, name, args...)
}

type limitedBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		if !b.truncated {
			b.truncated = true
			b.Buffer.WriteString("\n[thesis] log truncated\n")
		}
		return n, nil
	}
	b.Buffer.Write(p)
	return n, nil
}

// Hint trims a compile log to its first error line, for compact UI display.
func Hint(log []byte) string {
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "error") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
