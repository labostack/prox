package balancer

import (
	"sync"
	"testing"
)

func TestRoundRobin(t *testing.T) {
	targets := []string{"a:1", "b:2", "c:3"}
	rr := NewRoundRobin(targets)

	// Should cycle through targets in order.
	for cycle := 0; cycle < 3; cycle++ {
		for i, want := range targets {
			got := rr.Next()
			if got != want {
				t.Errorf("cycle %d, step %d: got %q, want %q", cycle, i, got, want)
			}
		}
	}
}

func TestRoundRobin_SingleTarget(t *testing.T) {
	rr := NewRoundRobin([]string{"only:1"})

	for i := 0; i < 5; i++ {
		if got := rr.Next(); got != "only:1" {
			t.Errorf("step %d: got %q, want %q", i, got, "only:1")
		}
	}
}

func TestRoundRobin_Done(t *testing.T) {
	rr := NewRoundRobin([]string{"a:1"})
	rr.Done("a:1") // no-op, should not panic
}

func TestRandom(t *testing.T) {
	targets := []string{"a:1", "b:2", "c:3"}
	r := NewRandom(targets)

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		got := r.Next()
		seen[got] = true
		valid := false
		for _, tgt := range targets {
			if got == tgt {
				valid = true
				break
			}
		}
		if !valid {
			t.Errorf("step %d: got %q which is not in targets", i, got)
		}
	}

	if len(seen) != len(targets) {
		t.Errorf("expected all %d targets to be selected, got %d", len(targets), len(seen))
	}
}

func TestRandom_SingleTarget(t *testing.T) {
	r := NewRandom([]string{"only:1"})

	for i := 0; i < 5; i++ {
		if got := r.Next(); got != "only:1" {
			t.Errorf("step %d: got %q, want %q", i, got, "only:1")
		}
	}
}

func TestRandom_Done(t *testing.T) {
	r := NewRandom([]string{"a:1"})
	r.Done("a:1") // no-op, should not panic
}

func TestLeastConn_BasicRouting(t *testing.T) {
	lc := NewLeastConn([]string{"a:1", "b:2", "c:3"})

	// All start at 0 — first call picks "a:1" (first with minimum).
	got := lc.Next()
	if got != "a:1" {
		t.Errorf("first call: got %q, want %q", got, "a:1")
	}

	// a:1=1, b:2=0, c:3=0 — picks "b:2".
	got = lc.Next()
	if got != "b:2" {
		t.Errorf("second call: got %q, want %q", got, "b:2")
	}

	// a:1=1, b:2=1, c:3=0 — picks "c:3".
	got = lc.Next()
	if got != "c:3" {
		t.Errorf("third call: got %q, want %q", got, "c:3")
	}

	// All at 1 — cycles back to "a:1".
	got = lc.Next()
	if got != "a:1" {
		t.Errorf("fourth call: got %q, want %q", got, "a:1")
	}
}

func TestLeastConn_DoneDecrementsCounter(t *testing.T) {
	lc := NewLeastConn([]string{"a:1", "b:2", "c:3"})

	// Fill up: a:1=1, b:2=1, c:3=1
	lc.Next() // a:1
	lc.Next() // b:2
	lc.Next() // c:3

	if c := lc.Conns("a:1"); c != 1 {
		t.Errorf("a:1 conns: got %d, want 1", c)
	}

	// Release b:2 → b:2=0
	lc.Done("b:2")

	if c := lc.Conns("b:2"); c != 0 {
		t.Errorf("b:2 conns after Done: got %d, want 0", c)
	}

	// Next should pick b:2 (lowest at 0).
	got := lc.Next()
	if got != "b:2" {
		t.Errorf("after Done: got %q, want %q", got, "b:2")
	}
}

func TestLeastConn_DoneUnknownTarget(t *testing.T) {
	lc := NewLeastConn([]string{"a:1"})
	lc.Done("unknown:99") // should not panic
}

func TestLeastConn_ImbalancedLoad(t *testing.T) {
	// Simulate: a=5, b=5, c=0 — new requests should go to c.
	lc := NewLeastConn([]string{"a:1", "b:2", "c:3"})

	// Fill evenly: 15 calls → a=5, b=5, c=5.
	for i := 0; i < 15; i++ {
		lc.Next()
	}

	// Release all c connections: c goes from 5 to 0.
	for i := 0; i < 5; i++ {
		lc.Done("c:3")
	}

	// a=5, b=5, c=0 → next 5 should all go to c.
	for i := 0; i < 5; i++ {
		got := lc.Next()
		if got != "c:3" {
			t.Errorf("step %d: got %q, want %q (c has least conns)", i, got, "c:3")
		}
	}

	// c=5 again, all equal → next goes to a (first with min).
	got := lc.Next()
	if got != "a:1" {
		t.Errorf("after equalization: got %q, want %q", got, "a:1")
	}
}

func TestLeastConn_ConnsHelper(t *testing.T) {
	lc := NewLeastConn([]string{"a:1", "b:2"})

	if c := lc.Conns("a:1"); c != 0 {
		t.Errorf("initial: got %d, want 0", c)
	}

	lc.Next() // a:1
	if c := lc.Conns("a:1"); c != 1 {
		t.Errorf("after Next: got %d, want 1", c)
	}

	// Unknown target returns 0.
	if c := lc.Conns("unknown:99"); c != 0 {
		t.Errorf("unknown target: got %d, want 0", c)
	}
}

func TestLeastConn_Concurrent(t *testing.T) {
	targets := []string{"a:1", "b:2", "c:3"}
	lc := NewLeastConn(targets)

	var wg sync.WaitGroup
	const goroutines = 100
	const requests = 100

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < requests; r++ {
				target := lc.Next()
				lc.Done(target)
			}
		}()
	}

	wg.Wait()

	// After all requests complete, all counters should be 0.
	for _, tgt := range targets {
		if c := lc.Conns(tgt); c != 0 {
			t.Errorf("target %q: expected 0 conns after completion, got %d", tgt, c)
		}
	}
}
