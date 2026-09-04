package deej

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

var procSetStdHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetStdHandle")

// STD_ERROR_HANDLE, i.e. (DWORD)-12
const stdErrorHandle = ^uintptr(11)

// redirectStderrToFile points the process' error handle at a file. The go runtime
// writes panics and fatal runtime errors straight to that handle and never through
// our logger, so in a -H=windowsgui build - which has no console attached at all -
// a crash otherwise leaves no trace whatsoever: the process simply vanishes and the
// log just stops mid-run
func redirectStderrToFile(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open crash log: %w", err)
	}

	// the runtime resolves this handle through GetStdHandle on every single write,
	// so replacing it now is enough - it doesn't have to happen before startup
	ret, _, callErr := syscall.SyscallN(procSetStdHandle.Addr(), stdErrorHandle, file.Fd())
	if ret == 0 {
		file.Close()

		return fmt.Errorf("set stderr handle: %w", callErr)
	}

	// keeps the file from being collected, and routes anything written through the
	// os package to the same place
	os.Stderr = file

	// a marker per run, so a file containing nothing but markers means no crash
	fmt.Fprintf(file, "\n--- deej started %s ---\n", time.Now().Format("2006-01-02 15:04:05"))

	return nil
}
