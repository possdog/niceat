package main

import (
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLabelLocksSerializeSameLabel is the property the lock exists for:
// concurrent acquires of the same (domain, label) never overlap.  Without
// it, two registrations of one label could both pass the "no record" check
// and stack two TXT records on a single _atproto name.
func TestLabelLocksSerializeSameLabel(t *testing.T) {
	var ll labelLocks

	var holders atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				unlock := ll.acquire(testDomain, "shared")
				if n := holders.Add(1); n != 1 {
					t.Errorf("lock held by %d goroutines at once", n)
				}
				runtime.Gosched() // widen the window for a broken lock to trip
				holders.Add(-1)
				unlock()
			}
		}()
	}
	wg.Wait()
}

// TestLabelLocksDistinctStripesIndependent checks that labels landing on
// different stripes do not serialize with each other.  Labels that collide
// on one stripe merely wait on each other (harmless extra contention), so
// the collision case is intentionally not asserted.
func TestLabelLocksDistinctStripesIndependent(t *testing.T) {
	var ll labelLocks

	// Pick two labels known to sit on different stripes, using the same
	// pure function acquire uses.
	first := "aaaa"
	second := "bbbb"
	for i := 0; labelStripe(testDomain, second) == labelStripe(testDomain, first); i++ {
		if i > 4*labelLockStripes {
			t.Fatalf("could not find two labels on different stripes")
		}
		second = fmt.Sprintf("bbbb%d", i)
	}

	unlockFirst := ll.acquire(testDomain, first)
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlockSecond := ll.acquire(testDomain, second)
		unlockSecond()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("labels on different stripes deadlocked on each other")
	}
	unlockFirst()
}

// TestLabelLocksBoundedTable encodes the memory-bound half of the red-team
// HIGH-2 fix: the table is a fixed-size array, so its footprint cannot grow
// with the number of distinct labels, unlike the never-shrinking per-key
// map it replaces.  The structural assertion (every field is an array — no
// map to grow per label) guards against a regression; the loop checks that
// however many labels pass through, the lock keeps working.
func TestLabelLocksBoundedTable(t *testing.T) {
	typ := reflect.TypeOf(labelLocks{})
	for i := 0; i < typ.NumField(); i++ {
		if k := typ.Field(i).Type.Kind(); k != reflect.Array {
			t.Errorf("labelLocks field %q is %v; only fixed-size arrays are allowed so the table cannot grow per label",
				typ.Field(i).Name, k)
		}
	}

	var ll labelLocks
	for i := 0; i < 10_000; i++ {
		unlock := ll.acquire(testDomain, fmt.Sprintf("user%06d", i))
		unlock()
	}
}
