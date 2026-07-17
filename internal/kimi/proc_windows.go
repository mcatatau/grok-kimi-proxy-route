//go:build windows

package kimi

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject           = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procCloseHandle               = kernel32.NewProc("CloseHandle")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
)

const (
	JobObjectExtendedLimitInformation = 9
	JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x2000
)

type JOBOBJECT_EXTENDED_LIMIT_INFORMATION struct {
	BasicLimitInformation   JOBOBJECT_BASIC_LIMIT_INFORMATION
	IoLimitInfo             IO_COUNTERS
	LimitInfo               JOB_OBJECT_LIMIT
	ProcessMemoryLimit      uintptr
	JobMemoryLimit          uintptr
	PeakProcessMemoryUsed   uintptr
	PeakJobMemoryUsed       uintptr
}

type JOBOBJECT_BASIC_LIMIT_INFORMATION struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type IO_COUNTERS struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type JOB_OBJECT_LIMIT struct {
	LimitFlags uint32
}

var (
	jobMu       sync.Mutex
	jobHandles  = make(map[int]syscall.Handle) // pid -> job handle
)

func createJobObject() (syscall.Handle, error) {
	hJob, _, err := procCreateJobObject.Call(0, 0, 0)
	if hJob == 0 {
		return 0, fmt.Errorf("CreateJobObject failed: %v", err)
	}

	info := JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	ret, _, err := procSetInformationJobObject.Call(
		hJob,
		uintptr(JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if ret == 0 {
		procCloseHandle.Call(hJob)
		return 0, fmt.Errorf("SetInformationJobObject failed: %v", err)
	}

	return syscall.Handle(hJob), nil
}

func assignProcessToJobObject(hJob, hProcess syscall.Handle) error {
	ret, _, err := procAssignProcessToJobObject.Call(uintptr(hJob), uintptr(hProcess))
	if ret == 0 {
		return fmt.Errorf("AssignProcessToJobObject failed: %v", err)
	}
	return nil
}

func openProcess(pid uint32, access uint32) syscall.Handle {
	ret, _, _ := procOpenProcess.Call(uintptr(access), 0, uintptr(pid))
	if ret == 0 {
		return 0
	}
	return syscall.Handle(ret)
}

// SetupProcessJob creates a Windows Job Object and assigns the running process
// to it so all child processes (Chromium, etc.) are killed together.
// Must be called after cmd.Start() when cmd.Process is already set.
func SetupProcessJob(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return fmt.Errorf("process not started")
	}

	hJob, err := createJobObject()
	if err != nil {
		return err
	}

	hProcess := openProcess(uint32(cmd.Process.Pid), syscall.PROCESS_TERMINATE|0x0100|0x0040)
	if hProcess == 0 {
		closeHandle(hJob)
		return fmt.Errorf("OpenProcess failed for pid %d", cmd.Process.Pid)
	}
	defer closeHandle(hProcess)

	if err := assignProcessToJobObject(hJob, hProcess); err != nil {
		closeHandle(hJob)
		return err
	}

	jobMu.Lock()
	jobHandles[cmd.Process.Pid] = hJob
	jobMu.Unlock()

	return nil
}

// KillProcessTree terminates the Job Object associated with the process,
// killing all processes in the job including any Chromium children.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	pid := cmd.Process.Pid

	jobMu.Lock()
	hJob, ok := jobHandles[pid]
	jobMu.Unlock()

	if ok && hJob != 0 {
		_, _, _ = procTerminateJobObject.Call(uintptr(hJob), 1)
		closeHandle(hJob)

		jobMu.Lock()
		delete(jobHandles, pid)
		jobMu.Unlock()

		// Also try normal kill to ensure process exits.
		_ = cmd.Process.Kill()
		return nil
	}

	// Fallback to normal kill if no job handle.
	return cmd.Process.Kill()
}

func closeHandle(h syscall.Handle) {
	procCloseHandle.Call(uintptr(h))
}

// hideConsoleWindow prevents a black console flash when launching node/playwright.
func hideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

// fullyHideConsoleWindow uses CREATE_NO_WINDOW for headless mode.
func fullyHideConsoleWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
	cmd.SysProcAttr.HideWindow = true
}
