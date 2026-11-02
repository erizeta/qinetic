// Package leaktest checks that tests leave no goroutines behind, without a dependency outside qilat's.
package leaktest

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// VerifyTestMain runs the tests and fails the binary if goroutines started by them are still running
// once they finish, waiting a little for ones that are shutting down. It does not check under -fuzz, whose
// engine keeps goroutines of its own.
func VerifyTestMain(m *testing.M) {
	before := runtime.NumGoroutine()
	code := m.Run()
	if f := flag.Lookup("test.fuzz"); f != nil && f.Value.String() != "" {
		os.Exit(code)
	}
	if code == 0 {
		if leaked := wait(before); leaked != "" {
			fmt.Fprintf(os.Stderr, "leaktest: goroutines still running after the tests:\n%s\n", leaked)
			code = 1
		}
	}
	os.Exit(code)
}

// grace is how long goroutines that are shutting down get to finish.
var grace = 5 * time.Second

// wait returns the stacks of extra goroutines still running after the grace period, or "".
func wait(before int) string {
	deadline := time.Now().Add(grace)
	for {
		if runtime.NumGoroutine() <= before {
			return ""
		}
		if time.Now().After(deadline) {
			return stacks()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stacks returns every goroutine stack but the current one's.
func stacks() string {
	buf := make([]byte, 1<<20)
	all := string(buf[:runtime.Stack(buf, true)])
	parts := strings.Split(all, "\n\n")
	return strings.Join(parts[1:], "\n\n")
}
