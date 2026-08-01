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

// kill terminates the job's whole process tree. The handle is deliberately
// retained for the lifetime of a job that ends on its own: holding it is what
// lets KILL_ON_JOB_CLOSE reap stragglers when the process exits, and the
// number of live handles is bounded by the number of jobs a session starts.
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
