package proc

import "errors"

// ErrNoInvocationID means the process has no systemd invocation identity.
var ErrNoInvocationID = errors.New("process has no systemd INVOCATION_ID")

// ErrNotServiceMainProcess means /proc reports a different PPID than expected.
var ErrNotServiceMainProcess = errors.New("process is not the service main process")

// ErrNotSystemdParent means the service process's parent is not systemd.
var ErrNotSystemdParent = errors.New("process parent is not systemd")

// ErrNotUnitMainPID means systemd did not report this process as the unit MainPID.
var ErrNotUnitMainPID = errors.New("process is not the systemd unit MainPID")

// ErrUnsafeUnitCgroup marks a cgroup that is not a stoppable service unit.
var ErrUnsafeUnitCgroup = errors.New("own cgroup is not a stoppable service")
