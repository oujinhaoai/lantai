package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/clock"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/ownership"
	"github.com/oujinhaoai/lantai/internal/identity"
	"github.com/oujinhaoai/lantai/internal/identity/masterkey"
	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/platform/fsutil"
)

// 本文件是只能在服务端本机、持有数据根写锁时运行的实例命令：初始化、迁移、
// 只读诊断与单管理员离线恢复。它们直接打开数据根，不经网络接口，因此不提供
// 远程等价物；日常的身份与凭据管理走 REST 接口与人类授权。

func homeFlag(fs *flag.FlagSet) *string {
	return fs.String("home", os.Getenv("LANTAI_HOME"), "数据根目录（默认取环境变量 LANTAI_HOME）")
}

func reportStartup(stderr io.Writer, cmd string, err error) int {
	var se *operations.StartupError
	if errors.As(err, &se) {
		fmt.Fprintf(stderr, "lantai %s: the instance cannot be used as requested:\n", cmd)
		for _, r := range se.Reasons {
			fmt.Fprintf(stderr, "  - %s: %s\n", r.Code, r.Message)
		}
		return exitInvalid
	}
	if e, ok := errcode.As(err); ok {
		fmt.Fprintf(stderr, "lantai %s: %s: %s\n", cmd, e.Code, e.Message)
		return exitInvalid
	}
	fmt.Fprintf(stderr, "lantai %s: %v\n", cmd, err)
	return exitInternal
}

func newIdentity(inst *operations.Instance, key *masterkey.Key) (*identity.Service, error) {
	return identity.New(identity.Deps{
		Main: inst.DB(ownership.Main), Runtime: inst.DB(ownership.Runtime), Gate: inst.Gate(), Epochs: inst,
		Clock: inst.Clock(), IDs: inst.IDs(), Key: key, InstanceID: inst.InstanceID(), InstanceName: inst.Marker().Name,
	}, identity.Config{})
}

func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	name := fs.String("name", "", "实例名（默认取 config.yaml 或 lantai）")
	admin := fs.String("admin", "", "首个管理员的名称，例如 ada（必填）")
	display := fs.String("display-name", "", "管理员显示名")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	if *home == "" || *admin == "" {
		fmt.Fprintln(stderr, "usage: lantai init -home <data root> -admin <name> [-name <instance>] [-display-name <text>]")
		return exitUsage
	}
	if !identity.ValidName(authz.Human, *admin) {
		fmt.Fprintf(stderr, "lantai init: administrator name %q must be lowercase letters, digits, '-' or '_'\n", *admin)
		return exitUsage
	}
	inst, err := operations.Create(ctx, operations.CreateOptions{Options: operations.Options{Home: *home}, Name: *name})
	if err != nil {
		return reportStartup(stderr, "init", err)
	}
	defer inst.Close(context.Background())
	key, created, err := loadOrCreateKey(inst)
	if err != nil {
		fmt.Fprintln(stderr, "lantai init:", err)
		return exitInternal
	}
	svc, err := newIdentity(inst, key)
	if err != nil {
		fmt.Fprintln(stderr, "lantai init:", err)
		return exitInternal
	}
	if closedBy, err := svc.ClosedBy(ctx); err != nil {
		fmt.Fprintln(stderr, "lantai init:", err)
		return exitInternal
	} else if closedBy != "" {
		// 上一次初始化已建立管理员，但在写入实例标记前中断：只补完标记。
		// 恢复码已在当时展示；没有保存下来的，用 lantai recover-admin 重置后取得新码。
		if err := inst.Activate(ctx); err != nil {
			fmt.Fprintln(stderr, "lantai init:", err)
			return exitInternal
		}
		fmt.Fprintf(stdout, "initialization had already created administrator %s; instance %s is now active\n", closedBy, inst.InstanceID())
		fmt.Fprintln(stdout, "If its recovery codes were not saved, stop here and run lantai recover-admin to reset the authenticator and get new codes.")
		return exitOK
	}
	p := newPrompter(stderr)
	fmt.Fprintf(stderr, "Initializing instance %s in %s\n", inst.InstanceID(), inst.Layout().Home)
	pw, err := p.newPassword(*admin)
	if err != nil {
		fmt.Fprintln(stderr, "lantai init:", err)
		return exitInvalid
	}
	b, err := svc.BeginBootstrap(ctx, identity.BootstrapRequest{Name: *admin, DisplayName: *display, Password: pw})
	if err != nil {
		return reportStartup(stderr, "init", err)
	}
	showEnrollment(stdout, *admin, b.Enrollment)
	var res identity.BootstrapResult
	for attempt := 1; ; attempt++ {
		code, err := p.line("Enter the current 6-digit code from the authenticator: ")
		if err != nil {
			fmt.Fprintln(stderr, "lantai init:", err)
			return exitInvalid
		}
		res, err = b.Confirm(ctx, code)
		if err == nil {
			break
		}
		if errcode.CodeOf(err) != errcode.HumanProofRequired || attempt == 3 {
			return reportStartup(stderr, "init", err)
		}
		fmt.Fprintln(stderr, "that code did not match; check the device clock and enter the current code")
	}
	// 管理员与恢复码已提交：先展示恢复码，再写实例标记，中断时也不会丢失。
	showRecoveryCodes(stdout, res.RecoveryCodes)
	if err := inst.Activate(ctx); err != nil {
		fmt.Fprintln(stderr, "lantai init:", err)
		fmt.Fprintln(stderr, "the administrator exists; run lantai init again to finish activating the instance")
		return exitInternal
	}
	fmt.Fprintf(stdout, "\ninstance        %s (%s)\n", inst.InstanceID(), inst.Marker().Name)
	fmt.Fprintf(stdout, "administrator   %s (%s)\n", res.Principal.Name, res.Principal.ID)
	fmt.Fprintf(stdout, "master key      %s", masterkey.Path(inst.Config().SecretsDir))
	if created {
		fmt.Fprint(stdout, " (new)")
	}
	fmt.Fprintln(stdout, "\n\nBack up the master key separately from the data root: without it the authenticator cannot be verified.")
	fmt.Fprintln(stdout, "Initialization is now closed for this data root.")
	return exitOK
}

func loadOrCreateKey(inst *operations.Instance) (*masterkey.Key, bool, error) {
	dir := inst.Config().SecretsDir
	key, err := masterkey.Load(dir)
	if err == nil {
		return key, false, nil
	}
	if !errors.Is(err, masterkey.ErrMissing) {
		return nil, false, err
	}
	key, err = masterkey.Generate(inst.IDs(), inst.Clock(), rand.Reader)
	if err != nil {
		return nil, false, err
	}
	if err := masterkey.Save(dir, key); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

func showEnrollment(w io.Writer, account string, e identity.Enrollment) {
	fmt.Fprintf(w, "\nRegister this authenticator for %s on your phone. It is shown only now; do not give it to any agent.\n", account)
	fmt.Fprintf(w, "  secret  %s\n", e.Secret)
	fmt.Fprintf(w, "  uri     %s\n\n", e.URI)
}

func showRecoveryCodes(w io.Writer, codes []string) {
	fmt.Fprintln(w, "\nRecovery codes (each works once; store them offline, they are not shown again):")
	for _, c := range codes {
		fmt.Fprintf(w, "  %s\n", c)
	}
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	backup := fs.String("backup", "", "升级前已验证的共同备份目录；已绑定的中断迁移可省略")
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *home == "" {
		fmt.Fprintln(stderr, "usage: lantai migrate -home <data root> [-backup <complete backup>] [-json]")
		return exitUsage
	}
	inst, err := operations.Open(ctx, operations.Options{Home: *home})
	if err != nil {
		return reportStartup(stderr, "migrate", err)
	}
	defer inst.Close(context.Background())
	rep, err := inst.Migrate(ctx, *backup)
	if err != nil {
		reportStartup(stderr, "migrate", err)
		fmt.Fprintln(stderr, "the instance stays in maintenance; run lantai migrate again after fixing the cause")
		return exitInternal
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return exitInternal
		}
		return exitOK
	}
	if rep.Noop {
		fmt.Fprintln(stdout, "nothing to migrate; all databases satisfy the compatibility matrix")
		return exitOK
	}
	fmt.Fprintf(stdout, "migration run %s completed\n", rep.RunID)
	for _, db := range []ownership.Database{ownership.Main, ownership.Ledger, ownership.Runtime, ownership.Events, ownership.Index} {
		fmt.Fprintf(stdout, "  %-8s v%d -> v%d %v\n", db, rep.Before[db], rep.After[db], rep.Applied[db])
	}
	return exitOK
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *home == "" {
		fmt.Fprintln(stderr, "usage: lantai doctor -home <data root> [-json]")
		return exitUsage
	}
	rep, err := operations.Inspect(ctx, *home)
	if err != nil {
		fmt.Fprintln(stderr, "lantai doctor:", err)
		return exitInternal
	}
	code := exitOK
	if !rep.Compatible() {
		code = exitInvalid
	}
	if *asJSON {
		out := struct {
			operations.Report
			Compatible bool `json:"compatible"`
		}{rep, rep.Compatible()}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitInternal
		}
		return code
	}
	fmt.Fprintf(stdout, "data root    %s\n", rep.Home)
	if m := rep.Marker; m != nil {
		fmt.Fprintf(stdout, "instance     %s (%s) %s, data format %d, recovery epoch %d\n", m.InstanceID, m.Name, m.State, m.DataFormatVersion, m.RecoveryEpoch)
	}
	where := "local"
	if rep.FileSystem.Remote {
		where = "NETWORK"
	} else if !rep.FileSystem.Known {
		where = "unverified"
	}
	fmt.Fprintf(stdout, "file system  %s (%s), %s free, %s required\n", rep.FileSystem.Type, where, bytesText(rep.FreeBytes), bytesText(rep.MinFree))
	if h := rep.LockHolder; h != nil {
		fmt.Fprintf(stdout, "last lock    pid %d at %s (the lock may no longer be held)\n", h.PID, h.AcquiredAt)
	}
	if len(rep.Databases) > 0 {
		fmt.Fprintln(stdout, "databases")
		for _, d := range rep.Databases {
			state := "missing"
			if d.Present {
				state = fmt.Sprintf("v%d/%d", d.Applied, d.Latest)
			}
			fmt.Fprintf(stdout, "  %-8s %s\n", d.Database, state)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(stdout, "note         %s: %s\n", n.Code, n.Message)
	}
	if rep.Compatible() {
		fmt.Fprintln(stdout, "status       ok")
		return code
	}
	fmt.Fprintln(stdout, "status       not ready:")
	for _, p := range rep.Problems {
		fmt.Fprintf(stdout, "  - %s: %s\n", p.Code, p.Message)
	}
	return code
}

func bytesText(n uint64) string {
	const gib = 1 << 30
	if n >= gib {
		return fmt.Sprintf("%.1f GiB", float64(n)/gib)
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

// runRecoverAdmin 是单管理员场景的本机灾难恢复：恢复码也丢失时，由控制数据根
// 与主密钥的主机管理员在服务停止后重置口令与验证器。
func runRecoverAdmin(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("recover-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	admin := fs.String("admin", "", "要重置的管理员名称（必填）")
	note := fs.String("note", "offline recovery", "写入审计的原因")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *home == "" || *admin == "" {
		fmt.Fprintln(stderr, "usage: lantai recover-admin -home <data root> -admin <name> [-note <reason>]")
		return exitUsage
	}
	inst, err := operations.Open(ctx, operations.Options{Home: *home})
	if err != nil {
		return reportStartup(stderr, "recover-admin", err)
	}
	defer inst.Close(context.Background())
	for _, r := range inst.Readiness().Reasons {
		if r.Code == operations.CodeMigrationRequired || r.Code == operations.CodeMigrationIncomplete {
			fmt.Fprintln(stderr, "lantai recover-admin: run lantai migrate first:", r.Message)
			return exitInvalid
		}
	}
	key, err := masterkey.Load(inst.Config().SecretsDir)
	if err != nil {
		fmt.Fprintln(stderr, "lantai recover-admin: the master key is required to prove control of this instance:", err)
		return exitInvalid
	}
	svc, err := newIdentity(inst, key)
	if err != nil {
		fmt.Fprintln(stderr, "lantai recover-admin:", err)
		return exitInternal
	}
	if r := inst.Marker().Restore; r != nil && r.Stage != "reconciliation_required" {
		fmt.Fprintln(stderr, "lantai recover-admin: finish the restore credential invalidation/key step first")
		return exitInvalid
	}
	code := exitInternal
	err = inst.OfflineMaintenance(ctx, func(c context.Context) error {
		code = resetAdministrator(c, inst, svc, *admin, *note, stdout, stderr)
		return nil
	})
	if err != nil {
		return reportStartup(stderr, "recover-admin", err)
	}
	return code
}

func resetAdministrator(ctx context.Context, inst *operations.Instance, svc *identity.Service, admin, note string, stdout, stderr io.Writer) int {
	p := newPrompter(stderr)
	fmt.Fprintf(stderr, "This resets the password and authenticator of administrator %s on instance %s.\n", admin, inst.InstanceID())
	fmt.Fprintln(stderr, "All of their sessions, pending authorizations and recovery codes stop working.")
	confirm, err := p.line(fmt.Sprintf("Type \"RESET %s\" to continue: ", admin))
	if err != nil || confirm != "RESET "+admin {
		fmt.Fprintln(stderr, "lantai recover-admin: not confirmed; nothing was changed")
		return exitInvalid
	}
	pw, err := p.newPassword(admin)
	if err != nil {
		fmt.Fprintln(stderr, "lantai recover-admin:", err)
		return exitInvalid
	}
	o, err := svc.BeginOfflineReset(ctx, admin, pw)
	if err != nil {
		return reportStartup(stderr, "recover-admin", err)
	}
	showEnrollment(stdout, admin, o.Enrollment)
	var codes []string
	for attempt := 1; ; attempt++ {
		code, err := p.line("Enter the current 6-digit code from the new authenticator: ")
		if err != nil {
			fmt.Fprintln(stderr, "lantai recover-admin:", err)
			return exitInvalid
		}
		codes, err = o.Confirm(ctx, code, note)
		if err == nil {
			break
		}
		if errcode.CodeOf(err) != errcode.HumanProofRequired || attempt == 3 {
			return reportStartup(stderr, "recover-admin", err)
		}
		fmt.Fprintln(stderr, "that code did not match; enter the current code")
	}
	if err := appendLocalAudit(inst.Layout().Home, inst.InstanceID(), o.Principal(), note); err != nil {
		fmt.Fprintln(stderr, "lantai recover-admin: the reset is committed but the local audit line could not be written:", err)
	}
	showRecoveryCodes(stdout, codes)
	fmt.Fprintf(stdout, "\n%s can sign in again with the new password and authenticator once the server is started.\n", admin)
	return exitOK
}

// appendLocalAudit 在数据根的 logs/ 下追加一行本机审计记录（主库另有审计事件）。
func appendLocalAudit(home string, instance ids.ID, p identity.Principal, note string) error {
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "offline-recovery.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]any{
		"at": clock.Format(time.Now()), "instance_id": instance, "principal_id": p.ID, "name": p.Name,
		"pid": os.Getpid(), "note": strings.TrimSpace(note),
	})
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return fsutil.SyncDir(dir)
}
