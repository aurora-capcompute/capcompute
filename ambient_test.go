package capcompute

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	extism "github.com/extism/go-sdk"
)

// Kernel law #1 (no ambient authority): images granting ambient network or
// filesystem access are refused at kernel construction.
func TestNewKernelRejectsAmbientHosts(t *testing.T) {
	_, err := NewKernel[string, testPID](context.Background(), Config[string, testPID]{
		Image:        extism.Manifest{AllowedHosts: []string{"example.com"}},
		ProcessTable: newTestProcessTable(nil),
	})
	if !errors.Is(err, ErrAmbientAuthority) {
		t.Fatalf("error = %v, want ErrAmbientAuthority", err)
	}
}

func TestNewKernelRejectsAmbientPaths(t *testing.T) {
	_, err := NewKernel[string, testPID](context.Background(), Config[string, testPID]{
		Image:        extism.Manifest{AllowedPaths: map[string]string{"/tmp": "/tmp"}},
		ProcessTable: newTestProcessTable(nil),
	})
	if !errors.Is(err, ErrAmbientAuthority) {
		t.Fatalf("error = %v, want ErrAmbientAuthority", err)
	}
}

// Kernel law #1 (no ambient authority): the linear-memory cap is kernel-owned
// (Config.MaxMemoryPages). An image that also sets its own memory ceiling would
// silently override it (the SDK applies the manifest value last), so such an
// image is refused at construction.
func TestNewKernelRejectsImageMemoryOverride(t *testing.T) {
	_, err := NewKernel[string, testPID](context.Background(), Config[string, testPID]{
		Image:          extism.Manifest{Memory: &extism.ManifestMemory{MaxPages: 65536}},
		MaxMemoryPages: 256,
		ProcessTable:   newTestProcessTable(nil),
	})
	if !errors.Is(err, ErrImageMemoryOverride) {
		t.Fatalf("error = %v, want ErrImageMemoryOverride", err)
	}
}

// Kernel law #2 (determinism): the pinned WASI clock advances by a fixed step
// from a fixed origin, and two fresh instances read the identical sequence — a
// crash-replay is exactly a fresh instance, so un-journaled clock reads are safe.
func TestDeterministicClockRestartsIdentically(t *testing.T) {
	readN := func(c *deterministicClock, n int) ([]int64, []int64) {
		wall := make([]int64, n)
		nano := make([]int64, n)
		for i := 0; i < n; i++ {
			sec, frac := c.walltime()
			wall[i] = sec*1_000_000_000 + int64(frac)
			nano[i] = c.nanotime()
		}
		return wall, nano
	}

	firstWall, firstNano := readN(&deterministicClock{}, 8)
	secondWall, secondNano := readN(&deterministicClock{}, 8)

	if !slices.Equal(firstWall, secondWall) || !slices.Equal(firstNano, secondNano) {
		t.Fatalf("fresh clocks diverged:\nwall %v vs %v\nnano %v vs %v", firstWall, secondWall, firstNano, secondNano)
	}
	// Monotonic, fixed step: nanotime starts at 0 and advances one step per read.
	for i := 1; i < len(firstNano); i++ {
		if step := firstNano[i] - firstNano[i-1]; step != guestClockStepNanos {
			t.Fatalf("nanotime step %d = %d, want %d", i, step, guestClockStepNanos)
		}
	}
	if firstNano[0] != 0 {
		t.Fatalf("nanotime[0] = %d, want 0", firstNano[0])
	}
	// Walltime starts at the pinned epoch.
	if wantOrigin := guestEpochSec * 1_000_000_000; firstWall[0] != wantOrigin {
		t.Fatalf("walltime[0] = %d, want epoch %d", firstWall[0], wantOrigin)
	}
}

// Kernel law #2 (determinism): the ambient sources the kernel pins must
// produce identical sequences on every fresh instance, so a crash-replay
// observes exactly what the original run observed.
func TestDeterministicRandRestartsIdentically(t *testing.T) {
	first := &deterministicRand{state: 0x9E3779B97F4A7C15}
	second := &deterministicRand{state: 0x9E3779B97F4A7C15}

	a := make([]byte, 64)
	b := make([]byte, 64)
	if _, err := first.Read(a); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := second.Read(b); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("fresh instances diverged")
	}
	if bytes.Equal(a, make([]byte, 64)) {
		t.Fatal("rand produced all zeros")
	}
}
