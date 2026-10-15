package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/oujinhaoai/lantai/internal/application"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/operations"
)

func runBackupOnline(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup-online", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := homeFlag(fs)
	resume := fs.String("resume", "", "继续同一在线任务 ID")
	status := fs.String("status", "", "查询在线任务 ID")
	cancel := fs.String("cancel", "", "请求取消在线任务 ID；等待复制实际退出后释放维护")
	drain := fs.Int("drain-seconds", 120, "在途请求及后台写入排空期限")
	timeout := fs.Int("timeout-seconds", 7200, "整个任务期限")
	wait := fs.Bool("wait", true, "等待终态；断开 CLI 不取消任务")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	modes := 0
	for _, s := range []string{*resume, *status, *cancel} {
		if s != "" {
			if !ids.ID(s).Valid() {
				return exitUsage
			}
			modes++
		}
	}
	if *home == "" || fs.NArg() != 0 || modes > 1 {
		return exitUsage
	}
	marker, err := operations.ReadMarker(operations.Layout{Home: *home})
	if err != nil {
		return reportStartup(stderr, "backup-online", err)
	}
	client := application.BackupClient(marker.InstanceID)
	defer client.CloseIdleConnections()
	call := func(method, path string, body any) (application.BackupJob, error) {
		var j application.BackupJob
		var payload bytes.Buffer
		if body != nil {
			if e := json.NewEncoder(&payload).Encode(body); e != nil {
				return j, e
			}
		}
		req, e := http.NewRequestWithContext(ctx, method, "http://lantai-backup"+path, &payload)
		if e != nil {
			return j, e
		}
		req.Header.Set("X-Lantai-Instance", string(marker.InstanceID))
		req.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(req)
		if e != nil {
			return j, e
		}
		defer resp.Body.Close()
		dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode >= 300 {
			var failure struct {
				Error string `json:"error"`
			}
			_ = dec.Decode(&failure)
			return j, fmt.Errorf("backup control: HTTP %d: %s", resp.StatusCode, failure.Error)
		}
		err = dec.Decode(&j)
		return j, err
	}
	method, path := "POST", "/backup"
	var body any = application.BackupRequest{Resume: ids.ID(*resume), DrainSeconds: *drain, TimeoutSeconds: *timeout}
	if *status != "" {
		method, path, body = "GET", "/backup/"+*status, nil
	}
	if *cancel != "" {
		path, body = "/backup/"+*cancel+"/cancel", nil
	}
	j, err := call(method, path, body)
	if err != nil {
		return reportStartup(stderr, "backup-online", err)
	}
	if err = outputJSON(stdout, j); err != nil {
		return exitInternal
	}
	if *status != "" || !*wait {
		return exitOK
	}
	for j.State == "draining" || j.State == "backing_up" || j.State == "cancelling" {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stderr, "CLI detached; task remains owned by core; use -status or -cancel", j.ID)
			return exitInvalid
		case <-time.After(500 * time.Millisecond):
		}
		j, err = call("GET", "/backup/"+string(j.ID), nil)
		if err != nil {
			return reportStartup(stderr, "backup-online", err)
		}
	}
	if err = outputJSON(stdout, j); err != nil {
		return exitInternal
	}
	if j.State != "complete" {
		return exitInvalid
	}
	return exitOK
}
