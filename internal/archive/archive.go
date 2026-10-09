package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func ExtractTarGz(r io.Reader, root *os.Root) (err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() {
		if e := gz.Close(); err == nil {
			err = e
		}
	}()
	x := newExtractor(root)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, skip, err := cleanEntry(h.Name)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = x.mkdir(name)
		case tar.TypeReg:
			err = x.writeFile(name, tr, h.FileInfo().Mode().Perm())
		case tar.TypeSymlink:
			if safeSymlink(name, h.Linkname) {
				err = root.Symlink(h.Linkname, name)
			}
		}
		if err != nil {
			return err
		}
	}
}

func ExtractZip(zipPath string, root *os.Root) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	x := newExtractor(root)
	for _, f := range zr.File {
		name, skip, err := cleanEntry(f.Name)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := x.mkdir(name); err != nil {
				return err
			}
		case mode&fs.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			linkname := string(target)
			if !safeSymlink(name, linkname) {
				continue
			}
			if err := root.Symlink(linkname, name); err != nil {
				return err
			}
		default:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = x.writeFile(name, rc, mode.Perm())
			rc.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func PackageRoot(tmp string) (string, error) {
	for _, m := range []string{"extension.json", "skin.json"} {
		if _, err := os.Stat(filepath.Join(tmp, m)); err == nil {
			return tmp, nil
		}
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", errors.New("archive has no extension.json or skin.json")
	}
	sub := filepath.Join(tmp, entries[0].Name())
	for _, m := range []string{"extension.json", "skin.json"} {
		if _, err := os.Stat(filepath.Join(sub, m)); err == nil {
			return sub, nil
		}
	}
	return "", errors.New("archive has no extension.json or skin.json")
}

func Swap(src, dest string) error {
	old := dest + ".phuo-old"
	// a leftover from a crashed swap
	// Windows rename will not replace it
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(dest, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		os.Rename(old, dest)
		return err
	}
	return os.RemoveAll(old)
}

type extractor struct {
	root *os.Root
	dirs map[string]struct{}
	buf  []byte
}

func newExtractor(root *os.Root) *extractor {
	return &extractor{
		root: root,
		dirs: map[string]struct{}{".": {}},
		buf:  make([]byte, 32*1024),
	}
}

func (x *extractor) mkdir(name string) error {
	if _, ok := x.dirs[name]; ok {
		return nil
	}
	if err := x.root.MkdirAll(name, 0o755); err != nil {
		return err
	}
	for {
		if _, ok := x.dirs[name]; ok {
			return nil
		}
		x.dirs[name] = struct{}{}
		parent := path.Dir(name)
		if parent == name {
			return nil
		}
		name = parent
	}
}

func (x *extractor) writeFile(name string, r io.Reader, perm fs.FileMode) error {
	if err := x.mkdir(path.Dir(name)); err != nil {
		return err
	}
	f, err := x.root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm|0o600)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(f, r, x.buf)
	return errors.Join(err, f.Close())
}

func cleanEntry(name string) (string, bool, error) {
	name = path.Clean(name)
	if name == "" || name == "." {
		return "", true, nil
	}
	if path.IsAbs(name) || hasDotDot(name) {
		return "", false, errors.New("invalid archive path " + name)
	}
	return name, false, nil
}

func hasDotDot(name string) bool {
	for name != "" {
		var part string
		part, name, _ = strings.Cut(name, "/")
		if part == ".." {
			return true
		}
	}
	return false
}

func safeSymlink(name, linkname string) bool {
	if path.IsAbs(linkname) {
		return false
	}
	target := path.Join(path.Dir(name), linkname)
	return target != ".." && !strings.HasPrefix(target, "../")
}
