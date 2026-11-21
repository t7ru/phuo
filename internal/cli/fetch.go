package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/installer"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/ui"
	"golang.org/x/sync/errgroup"
)

type FetchCmd struct{}

func (c *FetchCmd) Run(ctx context.Context, cli *CLI) error {
	lockPath, err := project.FindLock(cli.Cwd)
	if err != nil {
		return err
	}
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		return err
	}
	cacheDir, err := project.CacheDir(cli.CacheDir, os.Getenv("PHUO_CACHE_DIR"))
	if err != nil {
		return envErr(err.Error())
	}
	rep := ui.New(ui.Options{
		JSON: cli.JSON, Silent: cli.Silent, Verbose: cli.Verbose,
		NoColor: cli.NoColor, NoProgress: cli.NoProgress,
	})

	type item struct {
		res                      source.Resolved
		fallback, integrity, raw string
		materialize, cached      bool
	}
	seen := make(map[string]bool, len(lock.Packages))
	items := make([]item, 0, len(lock.Packages))
	skipped := 0
	for _, key := range slices.Sorted(maps.Keys(lock.Packages)) {
		lp := lock.Packages[key]
		if lp.Archive == "" {
			skipped++
			continue
		}
		if seen[lp.Archive] {
			continue
		}
		seen[lp.Archive] = true
		res := source.Resolved{Name: key, Source: lp.Source, SHA: lp.SHA, Archive: lp.Archive}
		it := item{res: res, integrity: lp.Integrity, raw: lp.Integrity, fallback: source.FallbackArchive(lp.Source, lp.SHA, lp.Archive)}
		hosted, _ := source.ArchiveAt(lp.Source, lp.SHA)
		gitiles := source.IsGitilesArchive(lp.Source, lp.SHA, lp.Archive)
		tree := strings.HasPrefix(lp.Integrity, archive.TreeIntegrityPrefix)
		it.materialize = tree || gitiles || hosted == lp.Archive
		if tree || gitiles {
			it.raw = ""
		}
		items = append(items, it)
	}
	if skipped > 0 {
		rep.Info("%d package(s) have no archive (git or local)", skipped)
	}
	if len(items) == 0 {
		rep.Info("nothing to fetch")
		return nil
	}

	cached := 0
	missing := 0
	todo := items[:0]
	for _, it := range items {
		got, ok := fetch.Cached(cacheDir, it.res.Archive)
		it.cached = ok && (it.raw == "" || got == it.raw)
		if it.cached {
			cached++
			if it.materialize {
				todo = append(todo, it)
			}
			continue
		}
		if ok {
			// a mismatched entry would poison installs
			if err := fetch.Uncache(cacheDir, it.res.Archive); err != nil {
				return err
			}
		}
		if cli.Offline {
			if _, ok := fetch.Cached(cacheDir, it.fallback); it.fallback != "" && ok {
				it.res.Archive = it.fallback
				it.integrity, it.raw = "", ""
				it.cached, it.materialize = true, true
				cached++
			} else {
				missing++
			}
		}
		todo = append(todo, it)
	}
	if missing > 0 {
		return userErr(fmt.Sprintf("offline: %d archive(s) not cached", missing))
	}
	if cli.Offline {
		ctx = fetch.Offline(ctx)
	}
	if len(todo) == 0 {
		rep.Info("%d archive(s) already cached", cached)
		return nil
	}

	var done atomic.Int64
	var total atomic.Int64
	rep.Progress("fetching %d archives...", len(todo))
	defer rep.ClearProgress()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(cli.Jobs, 1))
	for _, it := range todo {
		g.Go(func() error {
			if !it.cached {
				var cw countWriter
				got, err := fetch.Download(gctx, it.res.Archive, cacheDir, &cw)
				if err != nil && it.fallback != "" && gctx.Err() == nil {
					it.res.Archive = it.fallback
					it.integrity, it.raw = "", ""
					it.materialize = true
					cw.n = 0
					got, err = fetch.Download(gctx, it.res.Archive, cacheDir, &cw)
				}
				if err != nil {
					return err
				}
				if it.raw != "" && got != it.raw {
					return errors.Join(fmt.Errorf("%s: integrity mismatch", it.res.Name), fetch.Uncache(cacheDir, it.res.Archive))
				}
				total.Add(cw.n)
			}
			if it.materialize {
				tmp, err := os.MkdirTemp("", "phuo-fetch-*")
				if err != nil {
					return err
				}
				defer os.RemoveAll(tmp)
				if _, err := installer.MaterializeArchive(gctx, it.res, tmp, cacheDir, it.integrity); err != nil {
					return err
				}
			}
			rep.Progress("fetched %d/%d archives...", done.Add(1), len(todo))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	if fetched := len(items) - cached; fetched > 0 {
		rep.Info("%d archive(s) fetched (%s), %d already cached", fetched, formatSize(total.Load()), cached)
	} else {
		rep.Info("%d archive(s) already cached", cached)
	}
	return nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
