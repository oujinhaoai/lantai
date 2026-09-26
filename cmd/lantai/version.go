package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"

	"github.com/oujinhaoai/lantai/internal/contract/execution"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
)

type versionInfo struct {
	Module    string              `json:"module"`
	Version   string              `json:"version"`
	Revision  string              `json:"vcs_revision,omitempty"`
	Time      string              `json:"vcs_time,omitempty"`
	Modified  bool                `json:"vcs_modified"`
	GoVersion string              `json:"go_version"`
	Platform  string              `json:"platform"`
	Contracts []string            `json:"contracts"`
	Protocols []execution.Support `json:"protocols"`
}

func runVersion(_ context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "lantai version: unexpected arguments")
		return exitUsage
	}
	info := versionInfo{Module: "github.com/oujinhaoai/lantai", Version: "(devel)", GoVersion: runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH, Protocols: execution.SupportMatrix()}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.Revision = s.Value
			case "vcs.time":
				info.Time = s.Value
			case "vcs.modified":
				info.Modified = s.Value == "true"
			}
		}
	}
	reg, err := schema.Default()
	if err != nil {
		fmt.Fprintln(stderr, "lantai version:", err)
		return exitInternal
	}
	for _, e := range reg.Entries() {
		info.Contracts = append(info.Contracts, e.Contract)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			return exitInternal
		}
		return exitOK
	}
	fmt.Fprintf(stdout, "lantai %s (%s, %s)\n", info.Version, info.GoVersion, info.Platform)
	if info.Revision != "" {
		dirty := ""
		if info.Modified {
			dirty = " (modified)"
		}
		fmt.Fprintf(stdout, "revision %s %s%s\n", info.Revision, info.Time, dirty)
	}
	fmt.Fprintln(stdout, "protocols:")
	for _, p := range info.Protocols {
		fmt.Fprintf(stdout, "  %-28s %-14s %s\n", p.Protocol, p.Status, p.Planned)
	}
	fmt.Fprintf(stdout, "contracts: %d registered (lantai schema list)\n", len(info.Contracts))
	return exitOK
}
