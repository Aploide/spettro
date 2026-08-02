//go:build windows

package jobs

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no process groups that can be signalled as a unit, so the Unix
// "kill -pid" trick has no equivalent. A background job is instead placed in a
// Job Object: terminating the job kills the interpreter and everything it
// spawned in one call, however deep the tree, and the KILL_ON_JOB_CLOSE limit
// makes the OS do the same when spettro exits — so a dev server can never be
// left orphaned holding a port.
var (
	jobObjectMu sync.Mutex
	jobObjects  = map[*exec.Cmd]windows.Handle{}
)

// detach starts the command in its own process group so a Ctrl-C delivered to
// spettro's console is not broadcast to background jobs.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// afterStart puts the freshly started process into a new Job Object. It has to
// run after Start because a process can only be assigned once it exists; the
// gap is a few microseconds against an interpreter that takes far longer to
// reach its first spawn, so in practice the whole tree is captured.
//
// Failure is not fatal: the job simply falls back to single-process kill,
// which is what this platform did before.
func afterStart(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	// The API takes the struct as a bare uintptr, which hides the pointer from
	// the collector for the duration of the call.
	runtime.KeepAlive(&info)
	if err != nil {
		windows.CloseHandle(handle)
		return
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(handle)
		return
	}
	defer windows.CloseHandle(proc)
	if err := windows.AssignProcessToJobObject(handle, proc); err != nil {
		windows.CloseHandle(handle)
		return
	}

	jobObjectMu.Lock()
	jobObjects[cmd] = handle
	jobObjectMu.Unlock()
}

// afterWait releases the job object once the interpreter has exited. The usual
// case is an empty job — nothing outlived the shell — and the handle is closed
// straight away rather than held for the rest of the session.
//
// When children did outlive their parent the handle is deliberately kept: it is
// what makes KILL_ON_JOB_CLOSE terminate them when spettro exits, so a dev
// server started by a wrapper script cannot be left holding a port. Those are
// the only entries that outlive their job, and there is at most one per job.
func afterWait(cmd *exec.Cmd) {
	jobObjectMu.Lock()
	defer jobObjectMu.Unlock()
	handle, ok := jobObjects[cmd]
	if !ok || jobHasLiveProcesses(handle) {
		return
	}
	delete(jobObjects, cmd)
	windows.CloseHandle(handle)
}

// jobHasLiveProcesses reports whether any process remains in the job. An
// unreadable count is treated as "yes", which keeps the handle and so the old
// behaviour.
func jobHasLiveProcesses(handle windows.Handle) bool {
	var info jobBasicAccountingInformation
	var retlen uint32
	err := windows.QueryInformationJobObject(
		handle,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		&retlen,
	)
	runtime.KeepAlive(&info)
	if err != nil {
		return true
	}
	return info.ActiveProcesses > 0
}

// jobBasicAccountingInformation mirrors
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which x/sys/windows does not declare.
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// kill terminates the job's whole process tree.
func kill(cmd *exec.Cmd) error {
	jobObjectMu.Lock()
	handle, ok := jobObjects[cmd]
	delete(jobObjects, cmd)
	jobObjectMu.Unlock()

	if ok {
		err := windows.TerminateJobObject(handle, 1)
		windows.CloseHandle(handle)
		if err == nil {
			return nil
		}
	}
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
