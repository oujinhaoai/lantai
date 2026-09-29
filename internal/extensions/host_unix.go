//go:build !windows

package extensions

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

const processTreeMode = "process_group"

// processTree owns a child started as the leader of a new process group. Killing
// the negative PID reaches descendants that stay in that group.
type processTree struct {
	cmd *exec.Cmd
	pid int
}

func startTree(dir string, argv, env []string, log *os.File) (*processTree, error) {
	cmd := &exec.Cmd{Path: argv[0], Args: argv, Env: env, Dir: dir, Stdout: log, Stderr: log}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &processTree{cmd: cmd, pid: cmd.Process.Pid}, nil
}

func (t *processTree) wait() waitResult {
	err := t.cmd.Wait()
	state := t.cmd.ProcessState
	if state == nil {
		return waitResult{}
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return waitResult{}
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return waitResult{}
	}
	return waitResult{exited: true, code: state.ExitCode()}
}

func (t *processTree) kill() error {
	err := syscall.Kill(-t.pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (t *processTree) release() {}
