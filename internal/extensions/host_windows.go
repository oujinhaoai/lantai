//go:build windows

package extensions

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processTreeMode = "job_object"

var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// processTree starts the child suspended, assigns it to a kill-on-close job
// object and only then resumes it, so no descendant can start outside the job.
type processTree struct {
	cmd     *exec.Cmd
	job     windows.Handle
	process windows.Handle
}

func startTree(dir string, argv, env []string, log *os.File) (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	cmd := &exec.Cmd{Path: argv[0], Args: argv, Env: env, Dir: dir, Stdout: log, Stderr: log}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP}
	if err = cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	t := &processTree{cmd: cmd, job: job}
	fail := func(err error) (*processTree, error) {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.release()
		return nil, err
	}
	t.process, err = windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	if err = windows.AssignProcessToJobObject(job, t.process); err != nil {
		return fail(err)
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(t.process)); status != 0 {
		return fail(errors.New("extensions: resume suspended entry failed"))
	}
	return t, nil
}

func (t *processTree) wait() waitResult {
	err := t.cmd.Wait()
	state := t.cmd.ProcessState
	var exit *exec.ExitError
	if state == nil || err != nil && !errors.As(err, &exit) {
		return waitResult{}
	}
	return waitResult{exited: true, code: state.ExitCode()}
}

func (t *processTree) kill() error {
	if t.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(t.job, 1)
}

func (t *processTree) release() {
	if t.process != 0 {
		windows.CloseHandle(t.process)
		t.process = 0
	}
	if t.job != 0 {
		// KILL_ON_JOB_CLOSE reclaims anything still attached to the job.
		windows.CloseHandle(t.job)
		t.job = 0
	}
}

func runDirRemovalRetryable(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
