package fs

import (
	"errors"
	"io"
	stdfs "io/fs"
	"os"
	"path/filepath"
	"sort"
)

// walkDirBatch is how many directory entries List holds at once. A
// directory with fewer entries is read whole and visited in name order,
// exactly as filepath.WalkDir does; a larger one (a flat wal/<dep>/<tli>/
// holding years of segment manifests can have tens of millions) is read and
// visited batch by batch instead of being loaded into memory in one piece.
// Variable so tests can force the batched path with a small tree.
var walkDirBatch = 4096

// walkDirBounded is filepath.WalkDir with bounded memory per directory.
// Within a batch entries are visited in name order; across the batches of an
// oversize directory they are not (callers of List must not rely on order).
// SkipDir / SkipAll behave as in WalkDir.
func walkDirBounded(root string, fn stdfs.WalkDirFunc) error {
	info, err := os.Lstat(root)
	if err != nil {
		err = fn(root, nil, err)
	} else {
		err = walkDirBoundedEntry(root, stdfs.FileInfoToDirEntry(info), fn)
	}
	if errors.Is(err, stdfs.SkipDir) || errors.Is(err, stdfs.SkipAll) {
		return nil
	}
	return err
}

func walkDirBoundedEntry(path string, d stdfs.DirEntry, fn stdfs.WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if errors.Is(err, stdfs.SkipDir) && d.IsDir() {
			err = nil
		}
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		// Second call, to report the ReadDir error (as WalkDir does).
		err = fn(path, d, err)
		if err != nil {
			if errors.Is(err, stdfs.SkipDir) {
				err = nil
			}
			return err
		}
		return nil
	}
	defer f.Close()
	for {
		ents, rerr := f.ReadDir(walkDirBatch)
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, e := range ents {
			if err := walkDirBoundedEntry(filepath.Join(path, e.Name()), e, fn); err != nil {
				if errors.Is(err, stdfs.SkipDir) {
					return nil
				}
				return err
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			if err := fn(path, d, rerr); err != nil {
				if errors.Is(err, stdfs.SkipDir) {
					err = nil
				}
				return err
			}
			return nil
		}
		if len(ents) < walkDirBatch {
			return nil
		}
	}
}
