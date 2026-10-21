package application

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
)

// RestorePackage needs only the complete package and a new target. The old key
// is reconstructed in memory, verified by the normal restore implementation,
// and never installed as the new instance's runtime key.
func RestorePackage(ctx context.Context, file string, opts operations.RestoreOptions, cfg identity.Config) (operations.Marker, error) {
	if opts.Home == "" {
		return operations.Marker{}, errors.New("restore package: target required")
	}
	if len(opts.TargetConfig) == 0 {
		opts.TargetConfig = []byte(`{"contract":"lantai.config/v1","secrets":{"dir":"secrets"},"lifecycle":{"scheduler":false}}`)
	}
	parent := opts.StagingDir
	if parent == "" {
		parent = filepath.Dir(opts.Home)
	}
	stage, err := os.MkdirTemp(parent, ".lantai-recovery-")
	if err != nil {
		return operations.Marker{}, err
	}
	defer os.RemoveAll(stage)
	var key *masterkey.Key
	info, err := operations.ExtractRecoveryPackage(ctx, file, filepath.Join(stage, "snapshot"), func(r io.Reader) error {
		var e error
		key, e = masterkey.ReadRecoveryMaterial(r)
		return e
	})
	if err != nil {
		return operations.Marker{}, err
	}
	if key == nil || key.ID() != info.Manifest.KeyID {
		return operations.Marker{}, errors.New("restore package: recovery material binding mismatch")
	}
	opts.BackupDir = filepath.Join(stage, "snapshot")
	return Restore(ctx, opts, key, cfg)
}
