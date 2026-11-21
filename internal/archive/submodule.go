package archive

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/gcfg/v2"
)

type Submodule struct {
	Path, URL string
}

func Submodules(root *os.Root) ([]Submodule, error) {
	info, err := root.Lstat(".gitmodules")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New(".gitmodules must be a regular file")
	}
	f, err := root.Open(".gitmodules")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var modules []Submodule
	index := map[string]int{}
	err = gcfg.ReadWithCallback(f, func(section, name, key, value string, _ bool) error {
		if !strings.EqualFold(section, "submodule") {
			return nil
		}
		i, ok := index[name]
		if !ok {
			i = len(modules)
			index[name] = i
			modules = append(modules, Submodule{})
		}
		switch strings.ToLower(key) {
		case "path":
			modules[i].Path = value
		case "url":
			modules[i].URL = value
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf(".gitmodules: %w", err)
	}
	paths := make(map[string]bool, len(modules))
	for _, m := range modules {
		_, err := filepath.Localize(m.Path)
		if err != nil || m.Path == "." || strings.ContainsAny(m.Path, "\\\x00") ||
			slices.ContainsFunc(strings.Split(m.Path, "/"), func(p string) bool { return strings.EqualFold(p, ".git") }) {
			return nil, fmt.Errorf(".gitmodules: invalid submodule path %q", m.Path)
		}
		if paths[m.Path] || m.URL == "" {
			return nil, fmt.Errorf(".gitmodules: duplicate path or missing URL for %q", m.Path)
		}
		paths[m.Path] = true
	}
	for _, m := range modules {
		for dir := path.Dir(m.Path); dir != "."; dir = path.Dir(dir) {
			if paths[dir] {
				return nil, fmt.Errorf(".gitmodules: overlapping submodule paths %q and %q", dir, m.Path)
			}
		}
	}
	return modules, nil
}

func (m Submodule) Open(root *os.Root) (*os.Root, error) {
	end := -1
	for part := range strings.SplitSeq(m.Path, "/") {
		end += len(part) + 1
		info, err := root.Lstat(m.Path[:end])
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("submodule path %q must be a directory", m.Path[:end])
		}
	}
	child, err := root.OpenRoot(m.Path)
	if err != nil {
		return nil, err
	}
	f, err := child.Open(".")
	if err != nil {
		child.Close()
		return nil, err
	}
	names, err := f.Readdirnames(2)
	f.Close()
	if err == nil || errors.Is(err, io.EOF) {
		for _, name := range names {
			if name != ".git" {
				return child, nil
			}
		}
		err = nil
	}
	child.Close()
	return nil, err
}

func CheckSubmodules(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, ".gitmodules")); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return checkSubmodules(root, 0)
}

const MaxSubmoduleDepth = 32

func checkSubmodules(root *os.Root, depth int) error {
	modules, err := Submodules(root)
	if err != nil {
		return err
	}
	for _, m := range modules {
		if depth == MaxSubmoduleDepth {
			return errors.New("submodule nesting limit exceeded")
		}
		child, err := m.Open(root)
		if err != nil {
			return err
		}
		if child == nil {
			return fmt.Errorf("submodule %s is empty or missing", m.Path)
		}
		err = checkSubmodules(child, depth+1)
		child.Close()
		if err != nil {
			return fmt.Errorf("submodule %s: %w", m.Path, err)
		}
	}
	return nil
}
