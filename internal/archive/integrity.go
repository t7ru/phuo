package archive

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const TreeIntegrityPrefix = "tree-sha256-"

func TreeIntegrity(dir string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		name, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		h.Write(binary.AppendUvarint(nil, uint64(len(name))))
		io.WriteString(h, name)
		switch {
		case d.IsDir():
			h.Write([]byte{'d'})
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			target = filepath.ToSlash(target)
			h.Write(binary.AppendUvarint([]byte{'l'}, uint64(len(target))))
			io.WriteString(h, target)
		default:
			info, err := d.Info()
			if err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			// archive metas and filesys perms vary across hosts and platforms
			h.Write(binary.AppendUvarint([]byte{'f'}, uint64(info.Size())))
			_, err = io.Copy(h, f)
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return TreeIntegrityPrefix + base64.StdEncoding.EncodeToString(h.Sum(nil)), nil
}
