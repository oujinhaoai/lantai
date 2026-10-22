package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/oujinhaoai/lantai/internal/localassistant"
)

func runAssistant(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "configure" || args[0] == "enroll" || args[0] == "worker") {
		return runAssistantDevice(ctx, args, stdout, stderr)
	}
	if len(args) > 0 && args[0] == "result" {
		f := flag.NewFlagSet("assistant result", flag.ContinueOnError)
		f.SetOutput(io.Discard)
		var workspace, jobID, resultSHA string
		f.StringVar(&workspace, "workspace", "", "explicit private workspace")
		f.StringVar(&jobID, "job-id", "", "original remote job ULID")
		f.StringVar(&resultSHA, "result-sha256", "", "exact SHA256 from observed result reference")
		if f.Parse(args[1:]) != nil || f.NArg() != 0 || workspace == "" || jobID == "" || resultSHA == "" {
			fmt.Fprintln(stderr, "usage: lantai assistant result --workspace ROOT --job-id REMOTE_ULID --result-sha256 SHA256")
			return 2
		}
		out, err := localassistant.InspectLocalResult(workspace, jobID, resultSHA)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if json.NewEncoder(stdout).Encode(out) != nil {
			return 3
		}
		return 0
	}
	const help = `usage: lantai assistant inspect|run --job PRIVATE_JOB.json --workspace ROOT
  inspect validates one job and prints its approval_digest and job_digest; it performs no network or writes.
  run requires --approve SHA256 [--session-file PRIVATE_SESSION.json] [--ca-file CA.pem].
  Job, input, state, working-copy and result paths stay within the explicit workspace.
  No shell, persistent device access, session issuance, deletion, or automatic retry.
  A nonzero run can have partial effects: preserve original plan/state/operation keys.
  device channel: configure --input PRIVATE_CONFIG --config NEW_CONFIG --workspace ROOT
                  enroll --config PRIVATE_CONFIG --workspace ROOT
                  worker --config PRIVATE_CONFIG --workspace ROOT [--once]
  Worker is explicitly started outbound HTTPS; each job needs device inspection and web approval.
  local read-only review: result --workspace ROOT --job-id REMOTE_ULID --result-sha256 SHA256
`
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, help)
		return 0
	}
	mode := args[0]
	f := flag.NewFlagSet("assistant "+mode, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var job string
	var opts localassistant.Options
	f.StringVar(&job, "job", "", "explicit private job file")
	f.StringVar(&opts.Workspace, "workspace", "", "explicit local workspace")
	f.StringVar(&opts.ApprovalDigest, "approve", "", "exact digest returned by inspect")
	f.StringVar(&opts.SessionFile, "session-file", "", "explicit private session file, never in job")
	f.StringVar(&opts.CAFile, "ca-file", "", "strict-TLS CA for this client only")
	if err := f.Parse(args[1:]); err != nil || f.NArg() != 0 || (mode != "inspect" && mode != "run") || job == "" || opts.Workspace == "" {
		fmt.Fprint(stderr, help)
		return 2
	}
	if mode == "inspect" {
		if opts.ApprovalDigest != "" || opts.SessionFile != "" || opts.CAFile != "" {
			fmt.Fprintln(stderr, "assistant: inspect does not accept approval or credential arguments")
			return 2
		}
		out, err := localassistant.InspectFile(job, opts.Workspace)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if json.NewEncoder(stdout).Encode(out) != nil {
			return 3
		}
		return 0
	}
	out, err := localassistant.RunFile(ctx, job, opts)
	if out.Schema != "" {
		if json.NewEncoder(stdout).Encode(out) != nil {
			return 3
		}
		if out.ExitCode != 0 {
			fmt.Fprintln(stderr, "assistant: action did not complete; retain original state and inspect the structured result")
		}
		return out.ExitCode
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return 3
}

func runAssistantDevice(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flag.NewFlagSet("assistant "+args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var workspace, config, input string
	var once bool
	f.StringVar(&workspace, "workspace", "", "explicit private workspace")
	f.StringVar(&config, "config", "", "private local worker config")
	if args[0] == "configure" {
		f.StringVar(&input, "input", "", "explicit private capability config input")
	}
	if args[0] == "worker" {
		f.BoolVar(&once, "once", false, "process one observation and exit")
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || workspace == "" || config == "" || (args[0] == "configure" && input == "") {
		fmt.Fprintln(stderr, "assistant: configure|enroll|worker requires --workspace ROOT --config PRIVATE_FILE; configure also needs --input")
		return 2
	}
	var result any
	var err error
	switch args[0] {
	case "configure":
		result, err = localassistant.ConfigureWorker(ctx, input, config, workspace)
	case "enroll":
		result, err = localassistant.EnrollDevice(ctx, config, workspace)
	case "worker":
		result, err = localassistant.WorkDevice(ctx, config, workspace, once)
	}
	if err != nil {
		fmt.Fprintln(stderr, "assistant: device channel stopped; preserve original private config, job, result and operation state")
		return 3
	}
	if json.NewEncoder(stdout).Encode(result) != nil {
		return 3
	}
	return 0
}
