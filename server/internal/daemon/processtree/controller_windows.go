//go:build windows

package processtree

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const processTreeFinishTimeout = 5 * time.Second

type controller struct {
	job windows.Handle
}

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func newController(cmd *exec.Cmd) (*controller, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("set KILL_ON_JOB_CLOSE: %w", err)
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	// Assign the Job Object before any child instruction can spawn a shim's
	// descendant outside it. Ownership failure remains fail-closed.
	cmd.SysProcAttr.CreationFlags |= 0x00000004 // CREATE_SUSPENDED
	return &controller{job: job}, nil
}

func (c *controller) attach(cmd *exec.Cmd) error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		return fmt.Errorf("open process: %w", err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(c.job, process); err != nil {
		return fmt.Errorf("assign process to job object: %w", err)
	}
	return resumeWorkflowProcess(cmd.Process.Pid)
}

func resumeWorkflowProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{}
	entry.Size = uint32(unsafe.Sizeof(entry))
	resumed := false
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if openErr != nil {
			return openErr
		}
		_, resumeErr := windows.ResumeThread(thread)
		_ = windows.CloseHandle(thread)
		if resumeErr != nil {
			return resumeErr
		}
		resumed = true
	}
	if !resumed {
		return fmt.Errorf("no suspended thread for workflow process %d", pid)
	}
	return nil
}

func (c *controller) stop(_ *exec.Cmd) error {
	if err := windows.TerminateJobObject(c.job, 1); err != nil {
		active, queryErr := c.activeProcesses()
		if queryErr == nil && active == 0 {
			return nil
		}
		return err
	}
	return nil
}

// Windows Job Objects have no SIGTERM equivalent; terminating the job is the
// graceful-attempt and hard-stop operation alike.
func (c *controller) interrupt(cmd *exec.Cmd) error { return c.stop(cmd) }

func (c *controller) finish(_ *exec.Cmd) error {
	active, err := c.activeProcesses()
	if err != nil {
		return err
	}
	if active > 0 {
		if err := windows.TerminateJobObject(c.job, 1); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(processTreeFinishTimeout)
	for time.Now().Before(deadline) {
		active, err = c.activeProcesses()
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process job still active after %s", processTreeFinishTimeout)
}

func (c *controller) activeProcesses() (uint32, error) {
	var info jobObjectBasicAccountingInformation
	if err := windows.QueryInformationJobObject(
		c.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return 0, fmt.Errorf("query job object members: %w", err)
	}
	return info.ActiveProcesses, nil
}

func (c *controller) close() {
	if c.job == 0 {
		return
	}
	_ = windows.CloseHandle(c.job)
	c.job = 0
}
