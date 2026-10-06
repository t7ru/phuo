package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync/atomic"

	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/project"
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
		key, url, want string
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
		items = append(items, item{key: key, url: lp.Archive, want: lp.Integrity})
	}
	if skipped > 0 {
		rep.Info("%d package(s) have no archive (git or local)", skipped)
	}
	if len(items) == 0 {
		rep.Info("nothing to fetch")
		return nil
	}

	cached := 0
	todo := items[:0]
	for _, it := range items {
		got, ok := fetch.Cached(cacheDir, it.url)
		if ok && (it.want == "" || got == it.want) {
			cached++
			continue
		}
		if ok {
			// a mismatched entry would poison installs
			if err := fetch.Uncache(cacheDir, it.url); err != nil {
				return err
			}
		}
		todo = append(todo, it)
	}
	if cli.Offline && len(todo) > 0 {
		return userErr(fmt.Sprintf("offline: %d archive(s) not cached", len(todo)))
	}
	if len(todo) == 0 {
		rep.Info("%d archive(s) already cached", cached)
		return nil
	}

	sizes := make([]int64, len(todo))
	var done atomic.Int64
	rep.Progress("fetching %d archives...", len(todo))
	defer rep.ClearProgress()
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(cli.Jobs, 1))
	for i, it := range todo {
		g.Go(func() error {
			var cw countWriter
			got, err := fetch.Download(gctx, it.url, cacheDir, &cw)
			if err != nil {
				return err
			}
			if it.want != "" && got != it.want {
				return errors.Join(fmt.Errorf("%s: integrity mismatch", it.key), fetch.Uncache(cacheDir, it.url))
			}
			sizes[i] = cw.n
			rep.Progress("fetched %d/%d archives...", done.Add(1), len(todo))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	var total int64
	for _, n := range sizes {
		total += n
	}
	rep.Info("%d archive(s) fetched (%s), %d already cached", len(todo), formatSize(total), cached)
	return nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
