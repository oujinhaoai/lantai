package main

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/identity/totp"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var secretLine = regexp.MustCompile(`(?m)^  secret  ([A-Z2-7]+)$`)

// interact 运行一个交互命令：先写入 before 中的各行，等输出里出现验证器种子后
// 再写入当前动态码，最后写入 after 中的各行。
func interact(t *testing.T, args []string, before []string, sendCode bool) (int, string, string) {
	t.Helper()
	pr, pw := io.Pipe()
	old := stdin
	stdin = pr
	defer func() { stdin = old }()
	var out, errOut syncBuf
	done := make(chan int, 1)
	go func() { done <- run(t.Context(), args, &out, &errOut); pw.Close() }()
	go func() {
		for _, l := range before {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
		}
		if !sendCode {
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if m := secretLine.FindStringSubmatch(out.String()); m != nil {
				raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(m[1])
				if err != nil {
					t.Error(err)
					return
				}
				io.WriteString(pw, totp.Code(raw, totp.Counter(time.Now()))+"\n")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Error("the command never showed an authenticator secret")
	}()
	select {
	case code := <-done:
		return code, out.String(), errOut.String()
	case <-time.After(60 * time.Second):
		t.Fatal("command did not finish")
		return 0, "", ""
	}
}

func TestInitDoctorMigrateAndOfflineRecovery(t *testing.T) {
	home := filepath.Join(t.TempDir(), "lantai")
	// 不依赖临时盘至少有 1 GiB 可用。
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("contract: lantai.config/v1\nstorage:\n  min_free_bytes: 1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, "init", "-home", home); code != exitUsage {
		t.Fatalf("init without -admin: %d", code)
	}
	if code, _, _ := runCLI(t, "init", "-home", home, "-admin", "Bad Name"); code != exitUsage {
		t.Fatalf("init with bad admin name: %d", code)
	}
	if code, out, _ := runCLI(t, "doctor", "-home", home); code != exitInvalid || !strings.Contains(out, "not_initialized") {
		t.Fatalf("doctor before init: %d %q", code, out)
	}

	code, out, errOut := interact(t, []string{"init", "-home", home, "-admin", "ada", "-name", "studio"},
		[]string{"admin password 1", "admin password 1"}, true)
	if code != exitOK {
		t.Fatalf("init: %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	codes := regexp.MustCompile(`(?m)^  ([0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4})$`).FindAllStringSubmatch(out, -1)
	if len(codes) != 10 || !strings.Contains(out, "Initialization is now closed") {
		t.Fatalf("init output:\n%s", out)
	}
	if strings.Contains(out, "admin password 1") || strings.Contains(errOut, "admin password 1") {
		t.Fatal("the password must never be echoed")
	}
	if _, err := os.Stat(filepath.Join(home, "secrets", "master.key")); err != nil {
		t.Fatalf("master key: %v", err)
	}

	// 已初始化：再次初始化被拒，诊断正常，迁移无事可做。
	if code, _, errOut := interact(t, []string{"init", "-home", home, "-admin", "eve"}, []string{"x", "x"}, false); code != exitInvalid || !strings.Contains(errOut, "already_initialized") {
		t.Fatalf("second init: %d %q", code, errOut)
	}
	if code, out, _ := runCLI(t, "doctor", "-home", home); code != exitOK || !strings.Contains(out, "status       ok") || !strings.Contains(out, "(studio) active") {
		t.Fatalf("doctor: %d\n%s", code, out)
	}
	code, out, _ = runCLI(t, "doctor", "-home", home, "-json")
	var rep struct {
		Compatible bool `json:"compatible"`
		Databases  []struct {
			Database string `json:"database"`
		} `json:"databases"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != exitOK || !rep.Compatible || len(rep.Databases) != 5 {
		t.Fatalf("doctor -json: %d %v %s", code, err, out)
	}
	if code, out, _ := runCLI(t, "migrate", "-home", home); code != exitOK || !strings.Contains(out, "nothing to migrate") {
		t.Fatalf("migrate: %d %q", code, out)
	}

	// 离线恢复：确认短语不对时什么都不改。
	if code, _, errOut := interact(t, []string{"recover-admin", "-home", home, "-admin", "ada"}, []string{"reset ada"}, false); code != exitInvalid || !strings.Contains(errOut, "nothing was changed") {
		t.Fatalf("unconfirmed recovery: %d %q", code, errOut)
	}
	code, out, errOut = interact(t, []string{"recover-admin", "-home", home, "-admin", "ada", "-note", "lost phone"},
		[]string{"RESET ada", "admin password 2", "admin password 2"}, true)
	if code != exitOK || !strings.Contains(out, "Recovery codes") {
		t.Fatalf("recover-admin: %d\n%s\n%s", code, out, errOut)
	}
	audit, err := os.ReadFile(filepath.Join(home, "logs", "offline-recovery.log"))
	if err != nil || !strings.Contains(string(audit), `"note":"lost phone"`) || strings.Contains(string(audit), "admin password") {
		t.Fatalf("local audit: %v %s", err, audit)
	}

	// 权威库缺失：诊断报告恢复诊断原因，初始化仍然关闭。
	for _, sfx := range []string{"", "-wal", "-shm"} {
		os.Remove(filepath.Join(home, "db", "main.db"+sfx))
	}
	if code, out, _ := runCLI(t, "doctor", "-home", home); code != exitInvalid || !strings.Contains(out, "database_missing") {
		t.Fatalf("doctor with missing main.db: %d\n%s", code, out)
	}
	if code, _, errOut := interact(t, []string{"init", "-home", home, "-admin", "eve"}, nil, false); code != exitInvalid || !strings.Contains(errOut, "already_initialized") {
		t.Fatalf("init with missing main.db: %d %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "migrate", "-home", home); code != exitInvalid || !strings.Contains(errOut, "database_missing") {
		t.Fatalf("migrate with missing main.db: %d %q", code, errOut)
	}
}
