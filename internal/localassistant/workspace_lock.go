package localassistant

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/client"
)

type workspaceLockKey struct{}

// The lock lasts for the explicitly started process/run. Kernel locks release on
// crash; the private lock inode is retained rather than deleted/replaced.
func lockWorkspace(ctx context.Context, workspace string) (context.Context, func(), error) {
	abs, err := filepath.Abs(workspace)
	if err != nil || workspace == "" {
		return ctx, nil, invalid("explicit workspace required")
	}
	if held, _ := ctx.Value(workspaceLockKey{}).(string); held == abs {
		return ctx, func() {}, nil
	}
	if err = noLinks(abs, false); err != nil {
		return ctx, nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return ctx, nil, invalid("workspace cannot be opened")
	}
	const name = ".lantai-assistant.lock"
	st, err := root.Lstat(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		root.Close()
		return ctx, nil, invalid("workspace lock cannot be inspected")
	}
	if err == nil && !st.Mode().IsRegular() {
		root.Close()
		return ctx, nil, invalid("workspace lock must be a private regular file")
	}
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		root.Close()
		return ctx, nil, invalid("workspace lock cannot be opened")
	}
	fail := func() (context.Context, func(), error) {
		f.Close()
		root.Close()
		return ctx, nil, invalid("workspace lock is unsafe or already held")
	}
	opened, err := f.Stat()
	if err != nil {
		return fail()
	}
	current, err := root.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return fail()
	}
	if _, err = client.ReadPrivate(filepath.Join(abs, name)); err != nil {
		return fail()
	}
	if err = lockWorkerFile(f); err != nil {
		return fail()
	}
	current, err = root.Lstat(name)
	if err != nil || !os.SameFile(opened, current) {
		unlockWorkerFile(f)
		return fail()
	}
	return context.WithValue(ctx, workspaceLockKey{}, abs), func() { unlockWorkerFile(f); f.Close(); root.Close() }, nil
}
