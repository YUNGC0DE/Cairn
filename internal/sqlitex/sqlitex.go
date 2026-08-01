// Package sqlitex provides the narrow read-only SQLite access cairn needs to
// read Cursor's chat stores.
//
// It shells out to the `sqlite3` CLI rather than linking a driver. Rationale:
// a cgo driver would break the static-binary promise, and the pure-Go driver is
// a multi-megabyte dependency for two SELECT statements. Everything goes through
// Query, so swapping in a driver later is a one-file change.
//
// Reads never touch the user's live database: the file and its -wal/-shm
// sidecars are snapshotted to a temp directory first, so an in-progress Cursor
// session cannot be corrupted and no lock is taken.
package sqlitex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	colSep = "\x1f"
	rowSep = "\x1e"
)

var (
	lookupOnce sync.Once
	binPath    string
)

// ErrUnavailable means no SQLite backend is usable on this machine.
var ErrUnavailable = errors.New("sqlitex: no sqlite3 binary found in PATH")

// Available reports whether queries can run.
func Available() bool { return bin() != "" }

func bin() string {
	lookupOnce.Do(func() {
		if p, err := exec.LookPath("sqlite3"); err == nil {
			binPath = p
		}
	})
	return binPath
}

// Query runs sql against a snapshot of dbPath and returns rows of column
// strings. BLOB columns must be wrapped in hex() by the caller.
func Query(dbPath, sql string) ([][]string, error) {
	exe := bin()
	if exe == "" {
		return nil, ErrUnavailable
	}
	snap, cleanup, err := snapshot(dbPath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	cmd := exec.Command(exe, "-batch", "-noheader",
		"-separator", colSep, "-newline", rowSep, snap, sql)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("sqlitex: %s: %w: %s", filepath.Base(dbPath), err, strings.TrimSpace(errb.String()))
	}
	var rows [][]string
	for _, r := range strings.Split(out.String(), rowSep) {
		if r == "" {
			continue
		}
		rows = append(rows, strings.Split(r, colSep))
	}
	return rows, nil
}

// snapshot copies a database and its write-ahead sidecars to a temp directory.
func snapshot(dbPath string) (path string, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "cairn-sqlite-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	base := filepath.Base(dbPath)
	dst := filepath.Join(dir, base)
	if err := copyFile(dbPath, dst); err != nil {
		cleanup()
		return "", func() {}, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		// Missing sidecars are normal for an idle database.
		_ = copyFile(dbPath+suffix, dst+suffix)
	}
	return dst, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
