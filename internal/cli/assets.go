package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/catalog/pathrule"
	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/yamljson"
)

const assetHelp = `素材管理（现有公共 API 与显式本地副本，不直接访问服务端数据）：
  asset profile --input REQUEST [--output FILE]
  asset find --input REQUEST --server ORIGIN --session-file FILE
  asset workset plan|pull --input WORKSET --directory DIR [--purpose archive_review]
  asset import plan --input INPUT --directory SOURCE --output PLAN
  asset import apply --input PLAN --directory SOURCE --state STATE_DIR --output REPORT
  asset import verify --input PLAN --report REPORT --directory SOURCE --state STATE_DIR --destination VERIFY_DIR
  asset references plan --input INPUT --directory NOTES --output PLAN
  asset references copy --input PLAN --directory NOTES --destination COPY --output RECEIPT
  asset references rollback --input PLAN --directory COPY --destination ROLLBACK --post-cutover-refs FILE --output RECEIPT
profile 输入为 {input: PushInput, profile: AssetProfile}；find 为 {filter: AssetFilter, options: AssetFindOptions}。
profile、import plan 和 references 不连接服务端；其他命令使用 --server、--session-file / LANTAI_SESSION_TOKEN。
公共选项：--ca-file PEM、--allow-http（仅loopback开发）、--output FILE（私有JSON结果）。
import 不移动/删除源文件；apply 是显式写入口，先审阅 plan；verify 写入独立验证目录。
references 仅按显式映射创建独立笔记副本；rollback 另建原文副本并保留显式声明的切换后固定版本引用。
退出码非零时仍可能输出逐项/部分结果；description_pending 不是完整批次成功，不要换state或重传修复。
`

type assetOptions struct {
	server, session, caFile, input, directory, state, output, report, destination, purpose string
	postCutoverRefs                                                                        string
	allowHTTP                                                                              bool
	referencePlan                                                                          *client.ReferencePlan
}

func runAssets(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, assetHelp)
		return 0
	}
	name := args[0]
	args = args[1:]
	if name == "workset" || name == "import" || name == "references" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
			fmt.Fprint(stdout, assetHelp)
			return 0
		}
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			fmt.Fprint(stdout, assetHelp)
			return 2
		}
		name += "-" + args[0]
		args = args[1:]
	}
	var o assetOptions
	f := flag.NewFlagSet("asset "+name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&o.server, "server", os.Getenv("LANTAI_SERVER"), "HTTPS origin")
	f.StringVar(&o.session, "session-file", os.Getenv("LANTAI_SESSION"), "private session file")
	f.StringVar(&o.caFile, "ca-file", os.Getenv("LANTAI_CA_FILE"), "private CA bundle")
	f.BoolVar(&o.allowHTTP, "allow-http", false, "explicit loopback development")
	// Asset commands always return JSON; accept the common CLI switch so callers
	// can compose them with existing remote commands without a separate mode.
	f.Bool("json", true, "JSON result (always enabled)")
	f.StringVar(&o.input, "input", "", "JSON/YAML input")
	f.StringVar(&o.directory, "directory", "", "source or working copy directory")
	f.StringVar(&o.state, "state", "", "private batch state directory")
	f.StringVar(&o.output, "output", "", "private result JSON")
	f.StringVar(&o.report, "report", "", "prior import report")
	f.StringVar(&o.destination, "destination", "", "independent verification destination")
	f.StringVar(&o.purpose, "purpose", "archive_review", "read purpose")
	f.StringVar(&o.postCutoverRefs, "post-cutover-refs", "", "JSON/YAML array of fixed version references to retain")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, assetHelp)
			return 0
		}
		return writeError(stderr, usage("invalid asset command arguments; use asset --help"))
	}
	if f.NArg() != 0 {
		return writeError(stderr, usage("unexpected asset command positional arguments"))
	}
	// Decode this once before pinning an output: rollback's original source
	// root is in the plan, not --directory (which names the managed copy).
	if name == "references-copy" || name == "references-rollback" {
		var plan client.ReferencePlan
		if err := decodeAssetInput(o.input, &plan); err != nil {
			return writeError(stderr, err)
		}
		o.referencePlan = &plan
	}
	var output *client.PinnedPrivateOutput
	var outputParent string
	if o.output != "" {
		for _, source := range []string{o.input, o.report, o.postCutoverRefs} {
			if source != "" && sameAssetFile(o.output, source) {
				return writeError(stderr, usage("output must not overwrite an input or prior report"))
			}
		}
		var protected []string
		if strings.HasPrefix(name, "import-") || strings.HasPrefix(name, "references-") {
			protected = []string{o.directory, o.state, o.destination}
		} else if strings.HasPrefix(name, "workset-") {
			protected = []string{o.directory}
		}
		if o.referencePlan != nil {
			protected = append(protected, o.referencePlan.SourceRoot)
		}
		for _, root := range protected {
			if root != "" {
				if err := assetOutputOutside(o.output, root); err != nil {
					return writeError(stderr, err)
				}
			}
		}
		var err error
		outputParent, err = assetCanonicalPath(filepath.Dir(o.output))
		if err != nil {
			return writeError(stderr, err)
		}
		physicalOutput := filepath.Join(outputParent, filepath.Base(o.output))
		// The requested parent may have changed while its aliases were being
		// resolved. Check the physical path again before opening it for writes.
		for _, root := range protected {
			if root != "" {
				if err = assetOutputOutside(physicalOutput, root); err != nil {
					return writeError(stderr, err)
				}
			}
		}
		output, err = client.OpenPinnedPrivateOutput(physicalOutput)
		if err != nil {
			return writeError(stderr, err)
		}
		defer output.Close()
	}
	value, err := executeAsset(ctx, name, o)
	if value != nil {
		raw, marshalErr := json.MarshalIndent(value, "", "  ")
		if marshalErr != nil {
			return writeError(stderr, marshalErr)
		}
		if o.output != "" {
			currentParent, saveErr := assetCanonicalPath(filepath.Dir(o.output))
			if saveErr == nil && currentParent != outputParent {
				saveErr = errors.New("asset: output parent changed during the operation")
			}
			if saveErr == nil {
				saveErr = output.Write(append(raw, '\n'))
			}
			if saveErr != nil {
				fmt.Fprintln(stderr, "asset: result could not be saved; retain the original state before retrying")
				return writeError(stderr, saveErr)
			}
		}
		if _, writeErr := stdout.Write(append(raw, '\n')); writeErr != nil {
			return 3
		}
	}
	if err != nil {
		return writeError(stderr, err)
	}
	return 0
}

func executeAsset(ctx context.Context, name string, o assetOptions) (any, error) {
	switch name {
	case "profile":
		var in struct {
			Input   client.PushInput `json:"input"`
			Profile json.RawMessage  `json:"profile"`
		}
		if err := decodeAssetInput(o.input, &in); err != nil {
			return nil, err
		}
		profile, err := client.DecodeAssetProfile(in.Profile)
		if err != nil {
			return nil, usage("invalid asset profile: " + err.Error())
		}
		return client.ApplyAssetProfile(in.Input, profile)
	case "import-plan":
		if o.directory == "" || o.output == "" {
			return nil, usage("import plan requires --directory SOURCE and --output PLAN")
		}
		var in client.ImportInput
		if err := decodeAssetInput(o.input, &in); err != nil {
			return nil, err
		}
		return client.PrepareImport(ctx, in, o.directory)
	case "references-plan", "references-copy", "references-rollback":
		if o.directory == "" || o.output == "" {
			return nil, usage("references requires --directory and --output")
		}
		if name == "references-plan" {
			var in client.ReferenceInput
			if err := decodeAssetInput(o.input, &in); err != nil {
				return nil, err
			}
			plan, err := client.PrepareReferencePlan(ctx, in, o.directory)
			if err == nil && !plan.Complete {
				err = errors.New("asset: reference plan has unresolved selected mappings; review the partial plan")
			}
			return plan, err
		}
		if o.destination == "" {
			return nil, usage("references copy/rollback requires --destination")
		}
		var plan client.ReferencePlan
		if o.referencePlan != nil {
			plan = *o.referencePlan
		} else if err := decodeAssetInput(o.input, &plan); err != nil {
			return nil, err
		}
		// Rollback's --directory names the managed copy, while the original
		// read-only source root remains frozen inside the reviewed plan.
		if plan.SourceRoot != "" {
			if err := assetOutputOutside(o.output, plan.SourceRoot); err != nil {
				return nil, err
			}
		}
		if name == "references-copy" {
			return client.CopyReferenceNotes(ctx, plan, o.directory, o.destination)
		}
		if o.postCutoverRefs == "" {
			return nil, usage("references rollback requires --post-cutover-refs (an explicit array, including [] when none)")
		}
		var refs []string
		if err := decodeAssetInput(o.postCutoverRefs, &refs); err != nil {
			return nil, err
		}
		if refs == nil {
			return nil, usage("post-cutover references must be an explicit array")
		}
		return client.RollbackReferenceNotes(ctx, plan, o.directory, o.destination, refs)
	case "find", "workset-plan", "workset-pull", "import-apply", "import-verify":
	default:
		return nil, usage("unknown asset command; use asset --help")
	}
	// Decode and validate all local request documents before authentication or HTTP.
	var find struct {
		Filter  client.AssetFilter      `json:"filter"`
		Options client.AssetFindOptions `json:"options"`
	}
	var workset client.WorksetInput
	var plan client.ImportPlan
	var report client.ImportReport
	switch name {
	case "find":
		if err := decodeAssetInput(o.input, &find); err != nil {
			return nil, err
		}
	case "workset-plan", "workset-pull":
		if o.directory == "" {
			return nil, usage("workset requires --directory")
		}
		if err := decodeAssetInput(o.input, &workset); err != nil {
			return nil, err
		}
		if err := client.ValidateWorkset(workset, o.directory); err != nil {
			return nil, err
		}
	case "import-apply", "import-verify":
		if o.directory == "" || o.state == "" || (name == "import-apply" && o.output == "") {
			return nil, usage("import apply/verify requires --directory SOURCE and --state STATE_DIR; apply also requires --output REPORT")
		}
		if err := decodeAssetInput(o.input, &plan); err != nil {
			return nil, err
		}
		if err := client.ValidateImportPlan(ctx, plan, o.directory); err != nil {
			return nil, err
		}
		if name == "import-verify" {
			if o.destination == "" {
				return nil, usage("import verify requires --destination")
			}
			if err := decodeAssetInput(o.report, &report); err != nil {
				return nil, err
			}
		}
	}
	c, err := assetClient(o)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	switch name {
	case "find":
		return c.FindAssets(ctx, find.Filter, find.Options)
	case "workset-plan":
		return c.PlanWorkset(ctx, workset, o.directory)
	case "workset-pull":
		return c.PullWorkset(ctx, workset, o.directory, o.purpose)
	case "import-apply":
		return c.ApplyImport(ctx, plan, o.directory, o.state)
	case "import-verify":
		return c.VerifyImport(ctx, plan, report, o.directory, o.state, o.destination, o.purpose)
	}
	panic("unreachable asset command")
}

func decodeAssetInput(path string, value any) error {
	raw, err := readInputBytes(path)
	if err != nil {
		return err
	}
	// Validate both forms with the existing strict JSON data-model rules. A
	// generated JSON plan also binds RawMessage bytes: converting its objects
	// through a map would reorder them and invalidate an unchanged plan.
	normalized, err := yamljson.ToJSON(raw)
	if err != nil {
		return usage("input must be a single valid JSON/YAML document")
	}
	if !json.Valid(raw) {
		raw = normalized
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(value); err != nil {
		return usage("invalid asset input: " + err.Error())
	}
	if err = d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return usage("asset input must contain one document")
	}
	return nil
}

func assetClient(o assetOptions) (*client.Client, error) {
	if o.server == "" {
		return nil, usage("--server or LANTAI_SERVER is required")
	}
	c, err := client.New(client.Config{BaseURL: o.server, AllowHTTP: o.allowHTTP, CAFile: o.caFile})
	if err != nil {
		return nil, usage(err.Error())
	}
	token := os.Getenv("LANTAI_SESSION_TOKEN")
	if token == "" && o.session != "" {
		var saved sessionFile
		if err = client.LoadState(o.session, &saved); err != nil {
			c.Close()
			return nil, err
		}
		if saved.Schema != "lantai.client-session/v1" || saved.Origin != c.Origin() {
			c.Close()
			return nil, usage("session file belongs to a different server")
		}
		token = saved.Token
	}
	if token == "" {
		c.Close()
		return nil, errcode.New(errcode.AuthRequired, "session credential is required")
	}
	if err = c.SetSessionToken(token); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func sameAssetFile(a, b string) bool {
	x, e1 := filepath.Abs(a)
	y, e2 := filepath.Abs(b)
	if e1 == nil && e2 == nil && x == y {
		return true
	}
	xs, e1 := os.Stat(a)
	ys, e2 := os.Stat(b)
	return e1 == nil && e2 == nil && os.SameFile(xs, ys)
}

func assetOutputOutside(output, source string) error {
	src, err := assetCanonicalPath(source)
	if err != nil {
		return err
	}
	out, err := assetCanonicalPath(output)
	if err != nil {
		return err
	}
	// macOS case-insensitive volumes can resolve different spellings to the
	// same directory while EvalSymlinks returns different strings. Keep output
	// protection conservative across the catalog's Unicode/case path identity.
	rel, err := filepath.Rel(pathrule.Key(src), pathrule.Key(out))
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return usage("asset output must be outside source, state, verification and working-copy directories")
	}
	return nil
}

func assetCanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	ancestor := abs
	for {
		real, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			suffix, relErr := filepath.Rel(ancestor, abs)
			if relErr != nil {
				return "", relErr
			}
			return filepath.Join(real, suffix), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", resolveErr
		}
		ancestor = parent
	}
}
