package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

// maxDocumentBytes 限制单个被校验文档的大小。
const maxDocumentBytes = 16 << 20

func runSchema(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: lantai schema list [-json] | lantai schema validate [-json] <contract> <file>...")
		return exitUsage
	}
	switch args[0] {
	case "list":
		return schemaList(args[1:], stdout, stderr)
	case "validate":
		return schemaValidate(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "lantai schema: unknown subcommand %q\n", args[0])
		return exitUsage
	}
}

func schemaList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	reg, err := schema.Default()
	if err != nil {
		fmt.Fprintln(stderr, "lantai schema:", err)
		return exitInternal
	}
	entries := reg.Entries()
	if *asJSON {
		type item struct {
			schema.Entry
			URI string `json:"uri"`
		}
		out := make([]item, len(entries))
		for i, e := range entries {
			out[i] = item{e, e.URI()}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return exitInternal
		}
		return exitOK
	}
	for _, e := range entries {
		fmt.Fprintf(stdout, "%-30s %-9s %s\n", e.Contract, e.Kind, e.Path)
	}
	return exitOK
}

type validation struct {
	File  string            `json:"file"`
	Valid bool              `json:"valid"`
	Error *errcode.Envelope `json:"error,omitempty"`
}

func schemaValidate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("schema validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "usage: lantai schema validate [-json] <contract> <file>...")
		return exitUsage
	}
	reg, err := schema.Default()
	if err != nil {
		fmt.Fprintln(stderr, "lantai schema:", err)
		return exitInternal
	}
	contract := fs.Arg(0)
	if err := reg.Check(contract); err != nil {
		fmt.Fprintf(stderr, "lantai schema: %v (see lantai schema list)\n", err)
		return exitUsage
	}
	code := exitOK
	var results []validation
	for _, file := range fs.Args()[1:] {
		if ctx.Err() != nil {
			return exitInternal
		}
		res, ioErr := validateFile(reg, contract, file)
		if ioErr != nil {
			fmt.Fprintf(stderr, "lantai schema: %s: %v\n", file, ioErr)
			return exitInternal
		}
		if !res.Valid && code == exitOK {
			code = exitInvalid
		}
		results = append(results, res)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return exitInternal
		}
		return code
	}
	for _, r := range results {
		if r.Valid {
			fmt.Fprintf(stdout, "ok    %s\n", r.File)
			continue
		}
		fmt.Fprintf(stdout, "FAIL  %s: %s\n", r.File, r.Error.Error.Message)
		for _, d := range r.Error.Error.Details {
			ptr := "/"
			if d.Pointer != nil && *d.Pointer != "" {
				ptr = *d.Pointer
			}
			fmt.Fprintf(stdout, "      %s [%s] %s\n", ptr, d.Reason, d.Message)
		}
	}
	return code
}

// validateFile 只把读文件失败当作 I/O 错误；解析或校验失败属于文档无效。
func validateFile(reg *schema.Registry, contract, file string) (validation, error) {
	res := validation{File: file}
	f, err := os.Open(file)
	if err != nil {
		return res, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDocumentBytes+1))
	if err != nil {
		return res, err
	}
	if len(data) > maxDocumentBytes {
		return res, fmt.Errorf("document larger than %d bytes", maxDocumentBytes)
	}
	switch strings.ToLower(filepath.Ext(file)) {
	case ".yaml", ".yml":
		err = reg.ValidateYAML(contract, data)
	default:
		err = reg.ValidateJSON(contract, data)
	}
	if err == nil {
		res.Valid = true
		return res, nil
	}
	var ve *schema.ValidationError
	var e *errcode.Error
	if errors.As(err, &ve) {
		e = ve.Err()
	} else {
		e = errcode.New(errcode.SchemaInvalid, "document is not well-formed").
			WithDetails(errcode.Detail{Reason: "parse_error", Message: err.Error()})
	}
	env := e.Envelope("")
	res.Error = &env
	return res, nil
}
