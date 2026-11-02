package leaktest

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWait(t *testing.T) {
	grace = 50 * time.Millisecond
	t.Cleanup(func() { grace = 5 * time.Second })
	before := runtime.NumGoroutine()

	done := make(chan struct{})
	go func() { <-done }()
	if leaked := wait(before); !strings.Contains(leaked, "TestWait.func") {
		t.Fatalf("leak not reported:\n%s", leaked)
	}
	close(done)

	finishing := make(chan struct{})
	go func() { time.Sleep(10 * time.Millisecond); close(finishing) }()
	if leaked := wait(before); leaked != "" {
		t.Fatalf("goroutine that finished within the grace period reported:\n%s", leaked)
	}
	<-finishing
}
