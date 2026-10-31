package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// SQLite serializes writes, but cannot choose which process owns source cursors
// or active configuration. CLI inspection and labeling do not take this lock.
func lockRuntimeDB(path string) (*flock.Flock, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime database: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if os.IsNotExist(err) {
		directory, dirErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if dirErr != nil {
			return nil, fmt.Errorf("resolve runtime database directory: %w", dirErr)
		}
		resolved = filepath.Join(directory, filepath.Base(absolute))
	} else if err != nil {
		return nil, fmt.Errorf("resolve runtime database: %w", err)
	}
	lock := flock.New(resolved+".runtime.lock", flock.SetPermissions(0600))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		lock.Close()
		return nil, fmt.Errorf("runtime database is in use or cannot be locked; stop the other mrmr runtime first")
	}
	return lock, nil
}
