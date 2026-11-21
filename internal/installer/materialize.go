package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/manifest"
	"github.com/t7ru/phuo/internal/source"
)

type archiveMaterializer struct{ down sync.Map }

type materializedArchive struct {
	root, integrity, hostWarn       string
	fallback, filled, integrityWarn bool
}

type revision struct{ repo, sha string }

func (pl *Plan) fetchArchive(ctx context.Context, it *workItem, cacheDir, integrity string) error {
	it.tmpDir = filepath.Join(pl.parentDir(it.res.Type), ".phuo-tmp", it.res.Name)
	if err := os.RemoveAll(it.tmpDir); err != nil {
		return err
	}
	pkg, err := pl.archives.prepare(ctx, it.res, it.tmpDir, cacheDir, integrity, pl.opts.Force)
	if err != nil {
		return err
	}
	it.pkgRoot, it.integrity = pkg.root, pkg.integrity
	it.fallback, it.filled, it.hostWarn, it.integrityWarn = pkg.fallback, pkg.filled, pkg.hostWarn, pkg.integrityWarn
	man, kind, err := manifest.Read(pkg.root)
	if err != nil {
		return err
	}
	it.man = man
	if kind != "" && kind != it.res.Type {
		it.res.Type = kind
		it.key = kind + "/" + it.res.Name
	}
	return nil
}

func MaterializeArchive(ctx context.Context, res source.Resolved, tmp, cacheDir, integrity string) (string, error) {
	var m archiveMaterializer
	pkg, err := m.prepare(ctx, res, tmp, cacheDir, integrity, false)
	return pkg.root, err
}

func (m *archiveMaterializer) prepare(ctx context.Context, res source.Resolved, tmp, cacheDir, want string, force bool) (_ materializedArchive, err error) {
	var pkg materializedArchive
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
			err = fmt.Errorf("%s: %w", res.Name, err)
		}
	}()
	alternate := source.FallbackArchive(res.Source, res.SHA, res.Archive)
	u, _ := url.Parse(res.Archive)
	var raw string
	var dlErr error
	if u != nil && alternate != "" {
		if _, down := m.down.Load(u.Host); down {
			_, cached := fetch.Cached(cacheDir, res.Archive)
			pkg.fallback = !cached
		}
	}
	used := res.Archive
	if !pkg.fallback {
		raw, dlErr = extractPackage(ctx, used, tmp, cacheDir)
		var transferErr *transfer
		if dlErr != nil && alternate != "" && ctx.Err() == nil && errors.As(dlErr, &transferErr) {
			pkg.fallback = true
			var he *fetch.HTTPError
			if u != nil && !errors.Is(dlErr, fetch.ErrOffline) &&
				(!errors.As(dlErr, &he) || he.Status == http.StatusTooManyRequests || he.Status >= 500) {
				if _, seen := m.down.LoadOrStore(u.Host, struct{}{}); !seen {
					pkg.hostWarn = fmt.Sprintf("%s is unreachable; phuo will try git archives", u.Host)
				}
			}
		}
	}
	if pkg.fallback {
		if err := os.RemoveAll(tmp); err != nil {
			return pkg, err
		}
		used = alternate
		raw, dlErr = extractPackage(ctx, used, tmp, cacheDir)
	}
	if dlErr != nil {
		return pkg, dlErr
	}
	gitiles := source.IsGitilesArchive(res.Source, res.SHA, used)
	tree := strings.HasPrefix(want, archive.TreeIntegrityPrefix)
	if !pkg.fallback && want != "" && !tree && !gitiles && want != raw {
		if !force {
			return pkg, errors.New("integrity mismatch")
		}
		pkg.integrityWarn = true
	}
	pkg.root, err = archive.PackageRoot(tmp)
	if err != nil {
		return pkg, err
	}
	root, err := os.OpenRoot(pkg.root)
	if err != nil {
		return pkg, err
	}
	defer root.Close()
	origin := func() (revision, error) { return revision{res.Source, res.SHA}, nil }
	pkg.filled, err = fillSubmodules(ctx, root, origin, cacheDir, 0)
	if err != nil {
		return pkg, err
	}
	pkg.integrity = raw
	switch {
	case pkg.fallback:
		// lock still points at the primary archive
		pkg.integrity = want
	case tree || gitiles || pkg.filled:
		pkg.integrity, err = archive.TreeIntegrity(pkg.root)
		if err != nil {
			return pkg, err
		}
		if tree && want != pkg.integrity {
			if !force {
				return pkg, errors.New("integrity mismatch")
			}
			pkg.integrityWarn = true
		}
	}
	return pkg, nil
}

func fillSubmodules(ctx context.Context, root *os.Root, origin func() (revision, error), cacheDir string, depth int) (bool, error) {
	modules, err := archive.Submodules(root)
	if err != nil {
		return false, err
	}
	var filled bool
	for _, sub := range modules {
		if depth == archive.MaxSubmoduleDepth {
			return false, errors.New("submodule nesting limit exceeded")
		}
		resolve := sync.OnceValues(func() (revision, error) {
			parent, err := origin()
			if err != nil {
				return revision{}, err
			}
			repo, err := source.SubmoduleSource(parent.repo, sub.URL)
			if err != nil {
				return revision{}, err
			}
			sha, err := source.Gitlink(ctx, parent.repo, parent.sha, sub.Path, cacheDir)
			return revision{repo, sha}, err
		})
		child, err := sub.Open(root)
		if err == nil && child == nil {
			err = extractSubmodule(ctx, root, sub, resolve, cacheDir)
			if err == nil {
				filled = true
				child, err = sub.Open(root)
				if err == nil && child == nil {
					err = errors.New("archive is empty")
				}
			}
		}
		if err == nil {
			var nested bool
			nested, err = fillSubmodules(ctx, child, resolve, cacheDir, depth+1)
			child.Close()
			filled = filled || nested
		}
		if err != nil {
			return false, fmt.Errorf("submodule %s: %w", sub.Path, err)
		}
	}
	return filled, nil
}

func extractSubmodule(ctx context.Context, root *os.Root, sub archive.Submodule, resolve func() (revision, error), cacheDir string) error {
	rev, err := resolve()
	if err != nil {
		return err
	}
	u, ok := source.ArchiveAt(rev.repo, rev.sha)
	if !ok {
		return fmt.Errorf("no archive for %s at %s", rev.repo, rev.sha)
	}
	if err := root.MkdirAll(sub.Path, 0o755); err != nil {
		return err
	}
	child, err := root.OpenRoot(sub.Path)
	if err != nil {
		return err
	}
	defer child.Close()
	_, err = extractDownload(ctx, u, child, cacheDir, !source.IsGitilesArchive(rev.repo, rev.sha, u))
	return err
}

type transfer struct{ error }

func (e *transfer) Unwrap() error { return e.error }

var errArchiveExtraction = errors.New("archive extraction failed")

func extractPackage(ctx context.Context, u, dir, cacheDir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return extractDownload(ctx, u, root, cacheDir, false)
}

func extractDownload(ctx context.Context, u string, root *os.Root, cacheDir string, wrapped bool) (string, error) {
	if strings.HasSuffix(strings.ToLower(u), ".zip") {
		f, err := os.CreateTemp("", "phuo-archive-*.zip")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		integrity, dlErr := fetch.Download(ctx, u, cacheDir, f)
		closeErr := f.Close()
		if dlErr != nil {
			return "", &transfer{dlErr}
		}
		if closeErr != nil {
			return "", closeErr
		}
		return integrity, archive.ExtractZip(f.Name(), root)
	}

	pr, pw := io.Pipe()
	xerr := make(chan error, 1)
	go func() {
		extract := archive.ExtractTarGz
		if wrapped {
			extract = archive.ExtractWrappedTarGz
		}
		err := extract(pr, root)
		pr.CloseWithError(errArchiveExtraction)
		xerr <- err
	}()
	integrity, dlErr := fetch.Download(ctx, u, cacheDir, pw)
	pw.CloseWithError(dlErr)
	extractErr := <-xerr
	if dlErr != nil && !errors.Is(dlErr, errArchiveExtraction) {
		return "", &transfer{dlErr}
	}
	return integrity, extractErr
}
