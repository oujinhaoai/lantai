//go:build !linux && !darwin

package operations

import "os"

const recoveryExportOpenFlags = 0

// Native private access (including the Windows owner/DACL) is checked by
// fsutil.CheckPrivateFile. Unix mode bits are not a Windows access model.
func recoveryExportMetadata(os.FileInfo) error { return nil }
