package service

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"git.benmandrew.com/me/thesis/internal/store"
	"git.benmandrew.com/me/thesis/internal/typst"
)

//go:embed assets/diff-markup.typ
var diffMarkup []byte

// Diff methods, in order of preference.
const (
	MethodTypdiff = "typdiff"
	MethodDiffPDF = "diff-pdf"
	MethodText    = "text"
)

const diffMarkupDir = ".thesis-diff"

func (s *Service) runDiff(ctx context.Context, id int64) error {
	d, err := s.Store.GetDiff(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case d.Status == store.Queued, d.Status == store.Building:
	case d.Status == store.OK && d.Pruned:
	case d.Status == store.Timeout && d.Attempts < s.Cfg.Limits.MaxAttempts:
	default:
		return nil
	}
	if err := s.Store.StartDiff(ctx, d.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.diffDir(d.A, d.B), 0o755); err != nil {
		return err
	}

	var log bytes.Buffer
	fmt.Fprintf(&log, "[thesis] diff %s..%s, entrypoint %s\n", d.A[:12], d.B[:12], d.Entry)
	timedOut := false
	note := func(res typst.Result, what string) {
		if res.Status == "timeout" {
			timedOut = true
		}
		fmt.Fprintf(&log, "\n[thesis] %s: %s\n", what, res.Status)
		log.Write(res.Log)
	}

	bA, errA := s.EnsureBuilt(ctx, d.A, d.Entry)
	bB, errB := s.EnsureBuilt(ctx, d.B, d.Entry)
	if errB != nil {
		// B's own config is broken or the entrypoint is gone: nothing to diff.
		fmt.Fprintf(&log, "[thesis] cannot build B: %v\n", errB)
	}
	bothBuilt := errA == nil && errB == nil && bA.Status == store.OK && bB.Status == store.OK

	method := ""
	var size int64
	if errB == nil {
		res, err := s.typdiff(ctx, d)
		if err != nil {
			s.Store.RequeueDiff(context.WithoutCancel(ctx), d.ID)
			return err
		}
		note(res, "method 1, typdiff markup")
		if res.Status == "ok" {
			method, size = MethodTypdiff, res.Size
		}
	}
	if method == "" && bothBuilt {
		res, err := s.diffPDF(ctx, d)
		if err != nil {
			s.Store.RequeueDiff(context.WithoutCancel(ctx), d.ID)
			return err
		}
		note(res, "method 2, diff-pdf layout overlay")
		if res.Status == "ok" {
			method, size = MethodDiffPDF, res.Size
		}
	}
	if bothBuilt {
		res, err := s.textDiff(ctx, d)
		if err != nil {
			s.Store.RequeueDiff(context.WithoutCancel(ctx), d.ID)
			return err
		}
		fmt.Fprintf(&log, "\n[thesis] method 3, text diff: %s\n", res.Status)
		if res.Status == "ok" && method == "" {
			method = MethodText
		}
		size += res.Size
	} else {
		fmt.Fprintf(&log, "\n[thesis] methods 2 and 3 need both builds to succeed\n")
	}

	st := store.OK
	switch {
	case method != "":
	case timedOut:
		st = store.Timeout
	default:
		st = store.Failed
	}
	if err := writeFileAtomic(s.DiffLogPath(d.A, d.B, d.Entry), log.Bytes()); err != nil {
		return err
	}
	s.Log.Info("diff", "a", d.A[:12], "b", d.B[:12], "entry", d.Entry, "status", st, "method", method)
	return s.Store.FinishDiff(context.WithoutCancel(ctx), d.ID, st, method, size)
}

// typdiff rewrites every .typ file that changed between A and B as markup
// source, in a worktree of B, and compiles B's entrypoint from it.
func (s *Service) typdiff(ctx context.Context, d store.Diff) (typst.Result, error) {
	bc, err := s.BuildConfig(ctx, d.B)
	if err != nil {
		return typst.Result{}, err
	}
	commit, err := s.Commit(ctx, d.B)
	if err != nil {
		return typst.Result{}, err
	}
	tree, release, err := s.checkout(ctx, "diff", d.B)
	if err != nil {
		return typst.Result{}, err
	}
	defer release()
	if _, err := os.Lstat(filepath.Join(tree, diffMarkupDir)); err == nil {
		return failed("the commit has its own %s/, which the diff would overwrite", diffMarkupDir), nil
	}
	oldDir, err := os.MkdirTemp(s.Scratch, "old-")
	if err != nil {
		return typst.Result{}, err
	}
	defer os.RemoveAll(oldDir)

	changes, err := s.Mirror.ChangedTyp(ctx, d.A, d.B)
	if err != nil {
		return typst.Result{}, err
	}
	var log bytes.Buffer
	for i, c := range changes {
		if c.Status == 'D' {
			continue
		}
		newPath := filepath.Join(tree, c.Path)
		if fi, err := os.Lstat(newPath); err != nil || !fi.Mode().IsRegular() {
			// A symlink or something stranger; leave B's file as it is.
			continue
		}
		oldContent, _, err := s.Mirror.ReadFile(ctx, d.A, c.Path)
		if err != nil {
			return typst.Result{}, err
		}
		oldPath := filepath.Join(oldDir, fmt.Sprintf("%d.typ", i))
		if err := os.WriteFile(oldPath, oldContent, 0o644); err != nil {
			return typst.Result{}, err
		}
		outPath := filepath.Join(oldDir, fmt.Sprintf("%d.out.typ", i))
		res, err := typst.Tool(ctx, s.Cfg.Limits.BuildTimeout, s.Cfg.Limits.MaxOutputBytes, outPath, s.home(),
			s.Tools.Typdiff, "--no-preamble", "--output", outPath, "--", oldPath, newPath)
		if err != nil {
			return typst.Result{}, err
		}
		if res.Status != "ok" {
			fmt.Fprintf(&log, "typdiff on %s: %s\n%s", c.Path, res.Status, res.Log)
			res.Log = log.Bytes()
			return res, nil
		}
		marked, err := os.ReadFile(outPath)
		if err != nil {
			return typst.Result{}, err
		}
		header := []byte("#import \"/" + diffMarkupDir + "/diff-markup.typ\": *\n")
		if err := os.WriteFile(newPath, append(header, marked...), 0o644); err != nil {
			return typst.Result{}, err
		}
		fmt.Fprintf(&log, "typdiff: %c %s\n", c.Status, c.Path)
	}
	if err := os.MkdirAll(filepath.Join(tree, diffMarkupDir), 0o755); err != nil {
		return typst.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(tree, diffMarkupDir, "diff-markup.typ"), diffMarkup, 0o644); err != nil {
		return typst.Result{}, err
	}

	out := s.DiffPath(d.A, d.B, d.Entry, MethodTypdiff)
	res, err := s.compile(ctx, bc.Typst, tree, d.Entry, bc.Fonts, commit.CommittedAt, out+".tmp")
	if err != nil {
		return res, err
	}
	res.Log = append(log.Bytes(), res.Log...)
	if res.Status == "ok" {
		return res, os.Rename(out+".tmp", out)
	}
	os.Remove(out + ".tmp")
	return res, nil
}

// diffPDF overlays the two builds with diff-pdf. It exits 1 when the PDFs
// differ, which is the expected case.
func (s *Service) diffPDF(ctx context.Context, d store.Diff) (typst.Result, error) {
	out := s.DiffPath(d.A, d.B, d.Entry, MethodDiffPDF)
	res, err := typst.Tool(ctx, s.Cfg.Limits.BuildTimeout, s.Cfg.Limits.MaxOutputBytes, out+".tmp", s.home(),
		s.Tools.DiffPDF, "--output-diff="+out+".tmp", s.BuildPath(d.A, d.Entry), s.BuildPath(d.B, d.Entry))
	if err != nil {
		return res, err
	}
	if res.Status == "failed" && res.ExitCode == 1 {
		res.Status = "ok"
	}
	fi, statErr := os.Stat(out + ".tmp")
	switch {
	case res.Status != "ok":
	case statErr != nil:
		res.Status = "failed"
	case fi.Size() > s.Cfg.Limits.MaxOutputBytes:
		res.Status = "failed"
		res.Log = append(res.Log, "\n[thesis] output exceeds the size cap\n"...)
	default:
		res.Size = fi.Size()
		return res, os.Rename(out+".tmp", out)
	}
	os.Remove(out + ".tmp")
	return res, nil
}

// textDiff is a unified diff of pdftotext -layout on both builds.
func (s *Service) textDiff(ctx context.Context, d store.Diff) (typst.Result, error) {
	dir, err := os.MkdirTemp(s.Scratch, "text-")
	if err != nil {
		return typst.Result{}, err
	}
	defer os.RemoveAll(dir)
	var txt [2]string
	for i, sha := range []string{d.A, d.B} {
		txt[i] = filepath.Join(dir, sha[:12]+".txt")
		res, err := typst.Tool(ctx, s.Cfg.Limits.BuildTimeout, s.Cfg.Limits.MaxOutputBytes, txt[i], s.home(),
			s.Tools.PDFToText, "-layout", "-enc", "UTF-8", s.BuildPath(sha, d.Entry), txt[i])
		if err != nil || res.Status != "ok" {
			return res, err
		}
	}
	res, err := typst.Tool(ctx, s.Cfg.Limits.BuildTimeout, 0, "", s.home(),
		s.Tools.Git, "diff", "--no-index", "--no-color", "--text", "-U3", "--", txt[0], txt[1])
	if err != nil {
		return res, err
	}
	// git diff exits 1 when the files differ and 0 when they match.
	if res.Status == "failed" && res.ExitCode == 1 || res.Status == "ok" {
		res.Status = "ok"
		res.Size = int64(len(res.Log))
		return res, writeFileAtomic(s.TextDiffPath(d.A, d.B, d.Entry), res.Log)
	}
	return res, nil
}

func failed(format string, args ...any) typst.Result {
	return typst.Result{Status: "failed", Log: fmt.Appendf(nil, "[thesis] "+format+"\n", args...)}
}
