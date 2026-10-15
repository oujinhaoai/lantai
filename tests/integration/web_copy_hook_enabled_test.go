//go:build lantai_test_hooks

package integration

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/application"
)

func installFixtureCopyHook(a *application.App, hook func(context.Context)) error {
	return a.InstallBackupCopyHookForTest(hook)
}
