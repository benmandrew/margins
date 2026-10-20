package typst

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// releases serves the official archives the dev shell fetched, laid out as
// GitHub lays them out.
func releases(t *testing.T) (url, target string) {
	t.Helper()
	dir, target := os.Getenv("THESIS_TEST_TYPST_RELEASES"), os.Getenv("THESIS_TEST_TYPST_TARGET")
	if dir == "" {
		t.Skip("THESIS_TEST_TYPST_RELEASES is unset; run inside nix develop")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	return srv.URL, target
}

func archiveHash(t *testing.T, version, target string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("THESIS_TEST_TYPST_RELEASES"), "v"+version, "typst-"+target+".tar.xz"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestDownloadVerifiesHash(t *testing.T) {
	url, target := releases(t)
	good := archiveHash(t, "0.15.1", target)
	m := &Manager{Dir: t.TempDir(), BaseURL: url, Target: target, Hashes: map[string]string{
		"0.15.1/" + target: good,
		"0.14.2/" + target: good, // deliberately the wrong archive's hash
	}}
	bin, err := m.Binary(context.Background(), "0.15.1")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("binary %s not executable: %v", bin, err)
	}
	if _, err := m.Binary(context.Background(), "0.14.2"); err == nil {
		t.Fatal("archive with a mismatched hash was accepted")
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "0.14.2", "typst")); err == nil {
		t.Fatal("rejected archive left a binary behind")
	}
	if _, err := m.Binary(context.Background(), "0.13.0"); !errors.Is(err, ErrNoHash) {
		t.Fatalf("unpinned version: %v", err)
	}
	if _, err := m.Binary(context.Background(), "../0.15.1"); err == nil {
		t.Fatal("hostile version accepted")
	}
}

func TestCompile(t *testing.T) {
	url, target := releases(t)
	m := &Manager{Dir: t.TempDir(), BaseURL: url, Target: target,
		Hashes: map[string]string{"0.15.1/" + target: archiveHash(t, "0.15.1", target)}}
	bin, err := m.Binary(context.Background(), "0.15.1")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "ok.typ"), []byte("= Hello\nWorld."), 0o644)
	os.WriteFile(filepath.Join(root, "bad.typ"), []byte("#nope()"), 0o644)
	os.WriteFile(filepath.Join(root, "slow.typ"), []byte("#let n = 0\n#for i in range(9000) { for j in range(9000) { for k in range(9000) { n += 1 } } }\n#n"), 0o644)
	os.WriteFile(filepath.Join(root, "escape.typ"), []byte(`#read("/../../../../etc/passwd")`), 0o644)
	out := t.TempDir()
	c := Compile{Binary: bin, Root: root, Timestamp: 1700000000, PackageCache: t.TempDir(), Home: t.TempDir(), Timeout: 30 * time.Second}

	for _, tt := range []struct {
		entry, want string
		timeout     time.Duration
	}{
		{"ok.typ", "ok", 30 * time.Second},
		{"bad.typ", "failed", 30 * time.Second},
		{"escape.typ", "failed", 30 * time.Second},
		{"slow.typ", "timeout", 2 * time.Second},
	} {
		c := c
		c.Entry, c.Out, c.Timeout = tt.entry, filepath.Join(out, tt.entry+".pdf"), tt.timeout
		res, err := c.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", tt.entry, err)
		}
		if res.Status != tt.want {
			t.Errorf("%s: status %s, want %s; log:\n%s", tt.entry, res.Status, tt.want, res.Log)
		}
	}

	// Two compiles of the same tree give the same bytes.
	c.Entry = "ok.typ"
	c.Out = filepath.Join(out, "again.pdf")
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(out, "ok.typ.pdf"))
	b, _ := os.ReadFile(c.Out)
	if string(a) != string(b) {
		t.Error("rebuild is not byte-identical")
	}

	// The output cap turns an oversized PDF into a failure.
	c.MaxOutput = 10
	c.Out = filepath.Join(out, "capped.pdf")
	res, _ := c.Run(context.Background())
	if res.Status != "failed" {
		t.Errorf("capped output: status %s", res.Status)
	}
}
