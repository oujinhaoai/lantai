//go:build !lantai_test_hooks

package integration

import (
	"context"
	"errors"

	"github.com/oujinhaoai/lantai/internal/application"
)

func installFixtureCopyHook(*application.App, func(context.Context)) error {
	return errors.New("browser fixture requires -tags=lantai_test_hooks")
}
