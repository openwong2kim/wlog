package app

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// openBrowser opens the given URL in the platform default browser. Failures are
// returned (the caller downgrades them to a warning — the UI is still reachable
// via the printed URL).
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 is the most reliable Windows opener that does not pop a shell
		// window and handles URLs without escaping pitfalls.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default: // linux, *bsd, etc.
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", url, err)
	}
	// Reap the child so it does not linger as a zombie; the opener exits quickly.
	go func() { _ = cmd.Wait() }()
	return nil
}

// isTerminal reports whether f is attached to a character device (a TTY) as
// opposed to a pipe or file. Stdlib-only so it adds no dependency and works on
// every supported platform; it is used to decide whether --tail emits ANSI.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
