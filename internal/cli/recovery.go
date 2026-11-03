package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

const recoveryHelp = `新目标恢复（独立安装认领，不使用普通会话）：
  recovery package-hash --package FILE
  recovery status|upload|restore|cancel|check|complete --server ORIGIN --claim-file FILE [--ca-file PEM]
  upload --package FILE --package-sha256 SHA256；restore/complete --input FILE
  --output FILE 私有保存状态；--allow-http 仅loopback开发。
管理员因素必须由本人在既有安装流程完成；check不等于接受，complete必须明确当前review与每项切换确认。
响应丢失先读同一新目标status，不换包、不反复restore；原实例隔离与捕获后未知不能由CLI证明。
`

func runRecovery(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, recoveryHelp)
		return 0
	}
	action := args[0]
	f := flag.NewFlagSet("recovery "+action, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var server, claimFile, ca, input, pack, hash, output string
	var allowHTTP bool
	f.StringVar(&server, "server", os.Getenv("LANTAI_SERVER"), "target HTTPS origin")
	f.StringVar(&claimFile, "claim-file", "", "existing private installation claim")
	f.StringVar(&ca, "ca-file", os.Getenv("LANTAI_CA_FILE"), "client CA bundle")
	f.StringVar(&input, "input", "", "explicit private JSON input")
	f.StringVar(&pack, "package", "", "exact private complete package")
	f.StringVar(&hash, "package-sha256", "", "reviewed package SHA256")
	f.StringVar(&output, "output", "", "private JSON result")
	f.BoolVar(&allowHTTP, "allow-http", false, "loopback development only")
	f.Bool("json", true, "JSON output")
	if err := f.Parse(args[1:]); err != nil || f.NArg() != 0 {
		return writeError(stderr, usage("invalid recovery arguments; use recovery --help"))
	}
	var pinned *client.PinnedPrivateOutput
	if output != "" {
		for _, path := range []string{claimFile, input, pack} {
			if path != "" && sameAssetFile(path, output) {
				return writeError(stderr, usage("output must not overwrite a claim, input or package"))
			}
		}
		var err error
		pinned, err = client.OpenBatchPrivateOutput(output, claimFile, input, pack)
		if err != nil {
			return writeError(stderr, err)
		}
		defer pinned.Close()
	}
	var response client.Response
	var err error
	if action == "package-hash" {
		var size int64
		hash, size, err = client.HashRecoveryPackage(ctx, pack)
		if err == nil {
			response.Body, _ = json.Marshal(map[string]any{"sha256": hash, "size": size})
		}
	} else {
		var claim struct {
			Schema     string `json:"schema"`
			InstanceID string `json:"instance_id"`
			Code       string `json:"code"`
		}
		if err := decodeAssetInput(claimFile, &claim); err != nil {
			return writeError(stderr, err)
		}
		raw, e := base64.RawURLEncoding.DecodeString(claim.Code)
		if claim.Schema != "lantai.first-start/v1" || !ids.ID(claim.InstanceID).Valid() || e != nil || len(raw) != 32 {
			return writeError(stderr, usage("original installation claim file required"))
		}
		r, e := client.NewRecoveryControl(client.Config{BaseURL: server, CAFile: ca, AllowHTTP: allowHTTP}, claim.Code)
		if e != nil {
			return writeError(stderr, e)
		}
		defer r.Close()
		switch action {
		case "status":
			response, err = r.Status(ctx)
		case "upload":
			response, err = r.Upload(ctx, pack, hash)
		case "restore":
			var in client.RecoveryRestoreInput
			if err = decodeAssetInput(input, &in); err == nil {
				response, err = r.Restore(ctx, in)
			}
		case "cancel":
			response, err = r.Cancel(ctx)
		case "check":
			response, err = r.Check(ctx)
		case "complete":
			var in client.RecoveryCompleteInput
			if err = decodeAssetInput(input, &in); err == nil {
				response, err = r.Complete(ctx, in)
			}
		default:
			err = usage("unknown recovery action; administrator factors remain in the existing human installation flow")
		}
	}
	if err != nil {
		return writeError(stderr, err)
	}
	if pinned != nil {
		if err := pinned.Write(append(response.Body, '\n')); err != nil {
			return writeError(stderr, err)
		}
	}
	if _, err := stdout.Write(append(response.Body, '\n')); err != nil {
		return 3
	}
	return 0
}
