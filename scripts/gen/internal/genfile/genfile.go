// Package genfile 为仓库内的生成器提供“写入或核对”两种模式。
//
// 生成器默认覆盖输出文件；带 -check 时只比较磁盘内容与本次生成结果，
// 不一致即失败，用于 CI 确认重生成无差异。
package genfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrStale 表示磁盘上的生成文件与生成结果不一致。
var ErrStale = errors.New("generated file is stale; run scripts/generate.sh")

// Output 写入或核对 path；check 为 true 时不修改文件。
func Output(path string, content []byte, check bool) error {
	if check {
		cur, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !bytes.Equal(cur, content) {
			return fmt.Errorf("%s: %w", path, ErrStale)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cur, err := os.ReadFile(path)
	if err == nil && bytes.Equal(cur, content) {
		return nil // 内容未变不改写，避免无谓的修改时间变化
	}
	return os.WriteFile(path, content, 0o644)
}

// Root 返回仓库根目录：从当前目录向上寻找同时含 go.mod 与 schemas/ 的目录。
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if isRoot(dir) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("cannot find repository root (directory with go.mod and schemas/)")
		}
		dir = parent
	}
}

func isRoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "schemas"))
	return err == nil && st.IsDir()
}
