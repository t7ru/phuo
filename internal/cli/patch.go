package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/patch"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/spec"
	"github.com/t7ru/phuo/internal/ui"
)

func (c *PatchCmd) Run(ctx context.Context, cli *CLI) error {
	if c.Name == "" {
		return userErr("package name required")
	}
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	key, err := p.Lock.Lookup(c.Name)
	if err != nil {
		return err
	}
	lp := p.Lock.Packages[key]
	dir := packageDir(p, key)

	switch {
	case c.Remove:
		return patchRemove(p, key, lp, rep)
	case c.Commit:
		return patchCommit(ctx, cli, p, key, lp, dir, c.PatchesDir, rep)
	default:
		return patchBegin(ctx, cli, p, key, lp, dir, rep)
	}
}

func patchBegin(ctx context.Context, cli *CLI, p *project.Project, key string, lp project.Package, dir string, rep *ui.Reporter) error {
	if _, err := exec.LookPath("git"); err != nil {
		return envErr("git is required")
	}
	if err := exec.CommandContext(ctx, "git", "--version").Run(); err != nil {
		return envErr("git is required")
	}
	sp, err := specForKey(p, key)
	if err != nil {
		return err
	}
	if sp.Kind == spec.Local {
		return userErr(fmt.Sprintf("%s is a file: package; edit the source instead", keyName(key)))
	}
	if lp.Archive == "" && (sp.Kind == spec.Git || sp.Kind == spec.GitHub || sp.Kind == spec.GitLab ||
		strings.Contains(lp.Spec, "git+") || looksLikeClone(lp)) {
		return userErr(fmt.Sprintf("%s: use git in the checkout", keyName(key)))
	}
	if lp.Archive == "" {
		return userErr(fmt.Sprintf("%s: no archive to patch against", keyName(key)))
	}
	cacheDir := p.CacheDir
	if cli.NoCache {
		cacheDir = ""
	}
	tmp, err := os.MkdirTemp("", "phuo-patch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := downloadLockArchive(ctx, lp, tmp, cacheDir); err != nil {
		return err
	}
	rel := strings.TrimPrefix(dir, p.Root+string(filepath.Separator))
	if rel == dir {
		rel = key
	}
	rep.Info("edit %s, then run phuo patch --commit %s", filepath.ToSlash(rel), keyName(key))
	return nil
}

func looksLikeClone(lp project.Package) bool {
	return lp.Archive == "" && lp.Source != "" && lp.Integrity == ""
}

func patchCommit(ctx context.Context, cli *CLI, p *project.Project, key string, lp project.Package, dir, patchesDir string, rep *ui.Reporter) error {
	if _, err := exec.LookPath("git"); err != nil {
		return envErr("git is required")
	}
	if lp.Archive == "" {
		return userErr(fmt.Sprintf("%s: no archive to diff against", keyName(key)))
	}
	cacheDir := p.CacheDir
	if cli.NoCache {
		cacheDir = ""
	}
	tmp, err := os.MkdirTemp("", "phuo-patch-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := downloadLockArchive(ctx, lp, tmp, cacheDir); err != nil {
		return err
	}
	pristine, err := archive.PackageRoot(tmp)
	if err != nil {
		return err
	}

	leftCopy := filepath.Join(tmp, ".left")
	rightCopy := filepath.Join(tmp, ".right")
	if err := copyTreeFiltered(pristine, leftCopy, false); err != nil {
		return err
	}
	if err := copyTreeFiltered(dir, rightCopy, false); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "diff", "--no-index", "--binary", leftCopy, rightCopy)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
			return fmt.Errorf("git diff: %w\n%s", err, out.Bytes())
		}
	}
	diff := patch.RewriteDiffPrefixes(out.String(), filepath.ToSlash(leftCopy), filepath.ToSlash(rightCopy))
	if strings.TrimSpace(diff) == "" {
		return userErr("no local changes to commit")
	}

	if patchesDir == "" {
		patchesDir = "patches"
	}
	absPatches := patchesDir
	if !filepath.IsAbs(absPatches) {
		absPatches = filepath.Join(p.Root, patchesDir)
	}
	if err := os.MkdirAll(absPatches, 0o755); err != nil {
		return err
	}
	name := keyName(key)
	patchFile := filepath.Join(absPatches, name+".patch")
	if err := os.WriteFile(patchFile, []byte(diff), 0o644); err != nil {
		return err
	}
	relPatch := filepath.ToSlash(filepath.Join(patchesDir, name+".patch"))
	if filepath.IsAbs(patchesDir) {
		rel, err := filepath.Rel(p.Root, patchFile)
		if err != nil {
			relPatch = patchFile
		} else {
			relPatch = filepath.ToSlash(rel)
		}
	}
	sum := sha256.Sum256([]byte(diff))
	integrity := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])

	if p.Manifest.PatchedDependencies == nil {
		p.Manifest.PatchedDependencies = map[string]string{}
	}
	p.Manifest.PatchedDependencies[key] = relPatch
	lp.Patch = integrity
	p.Lock.Packages[key] = lp
	if err := p.Save(); err != nil {
		return err
	}
	rep.Info("wrote %s", relPatch)
	return nil
}

func patchRemove(p *project.Project, key string, lp project.Package, rep *ui.Reporter) error {
	rel, ok := p.Manifest.PatchedDependencies[key]
	if ok {
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.Root, rel)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(p.Manifest.PatchedDependencies, key)
	}
	lp.Patch = ""
	p.Lock.Packages[key] = lp
	if err := p.Save(); err != nil {
		return err
	}
	rep.Info("removed patch for %s; next install/update will be unpatched", keyName(key))
	return nil
}

func downloadLockArchive(ctx context.Context, lp project.Package, tmp, cacheDir string) error {
	url := lp.Archive
	err := extractArchive(ctx, url, tmp, cacheDir)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "HTTP 404") || lp.SHA == "" || lp.Source == "" {
		return err
	}
	fb, ok := source.ArchiveAt(lp.Source, lp.SHA)
	if !ok {
		return err
	}
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	return extractArchive(ctx, fb, tmp, cacheDir)
}
