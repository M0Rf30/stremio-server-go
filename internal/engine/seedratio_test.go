package engine

import (
	"sync"
	"testing"
)

// fakeToggler records AllowDataUpload/DisallowDataUpload calls so the
// seed-ratio enforcer can be driven without a live torrent client.
type fakeToggler struct {
	mu        sync.Mutex
	allowed   int
	disallow  int
	lastCall  string
	callOrder []string
}

func (f *fakeToggler) AllowDataUpload() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed++
	f.lastCall = "allow"
	f.callOrder = append(f.callOrder, "allow")
}

func (f *fakeToggler) DisallowDataUpload() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disallow++
	f.lastCall = "disallow"
	f.callOrder = append(f.callOrder, "disallow")
}

func (f *fakeToggler) counts() (allow, disallow int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowed, f.disallow
}

// TestSeedRatioExceeded pins the ratio decision itself, including the two
// disabling cases (no cap configured, nothing downloaded yet).
func TestSeedRatioExceeded(t *testing.T) {
	tests := []struct {
		name         string
		ul, dl       int64
		maxRatio     float64
		wantExceeded bool
		wantRatio    float64
	}{
		{"unlimited by default", 1000, 10, 0, false, 0},
		{"negative cap treated as unlimited", 1000, 10, -1, false, 0},
		{"nothing downloaded is never blocked", 5000, 0, 1.0, false, 0},
		{"below cap", 50, 100, 1.0, false, 0.5},
		{"exactly at cap blocks", 100, 100, 1.0, true, 1.0},
		{"above cap blocks", 250, 100, 2.0, true, 2.5},
		{"fractional cap below", 40, 100, 0.5, false, 0.4},
		{"fractional cap at", 50, 100, 0.5, true, 0.5},
		{"zero upload", 0, 100, 0.5, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ratio := seedRatioExceeded(tc.ul, tc.dl, tc.maxRatio)
			if got != tc.wantExceeded {
				t.Errorf("seedRatioExceeded(%d, %d, %v) exceeded = %v; want %v",
					tc.ul, tc.dl, tc.maxRatio, got, tc.wantExceeded)
			}
			if ratio != tc.wantRatio {
				t.Errorf("seedRatioExceeded(%d, %d, %v) ratio = %v; want %v",
					tc.ul, tc.dl, tc.maxRatio, ratio, tc.wantRatio)
			}
		})
	}
}

// TestApplySeedRatioOnlyActsOnTransition verifies the enforcer calls into the
// torrent exactly once per state change, not once per janitor tick.
func TestApplySeedRatioOnlyActsOnTransition(t *testing.T) {
	e := &engine{infoHash: "abc"}
	f := &fakeToggler{}

	// Under the cap: no call at all, since engines start unblocked.
	for range 3 {
		e.applySeedRatio(f, 10, 100, 2.0)
	}
	if allow, disallow := f.counts(); allow != 0 || disallow != 0 {
		t.Fatalf("under cap: got allow=%d disallow=%d; want no calls", allow, disallow)
	}
	if e.uploadBlocked {
		t.Fatal("engine should not be blocked while under the cap")
	}

	// Cross the cap: exactly one disallow, however many ticks run.
	for range 3 {
		e.applySeedRatio(f, 250, 100, 2.0)
	}
	if allow, disallow := f.counts(); allow != 0 || disallow != 1 {
		t.Fatalf("over cap: got allow=%d disallow=%d; want allow=0 disallow=1", allow, disallow)
	}
	if !e.uploadBlocked {
		t.Fatal("engine should be blocked after exceeding the cap")
	}

	// Fall back under the cap (more data downloaded): exactly one resume.
	for range 3 {
		e.applySeedRatio(f, 250, 1000, 2.0)
	}
	if allow, disallow := f.counts(); allow != 1 || disallow != 1 {
		t.Fatalf("back under cap: got allow=%d disallow=%d; want allow=1 disallow=1", allow, disallow)
	}
	if e.uploadBlocked {
		t.Fatal("engine should be unblocked once the ratio falls back under the cap")
	}
}

// TestApplySeedRatioDisablingReleasesBlock verifies that setting the cap back to
// 0 (unlimited) resumes a torrent an earlier, lower cap had paused, rather than
// stranding it paused for the life of the process.
func TestApplySeedRatioDisablingReleasesBlock(t *testing.T) {
	e := &engine{infoHash: "abc"}
	f := &fakeToggler{}

	e.applySeedRatio(f, 500, 100, 1.0)
	if !e.uploadBlocked {
		t.Fatal("expected upload to be blocked at ratio 5.0 with cap 1.0")
	}

	e.applySeedRatio(f, 500, 100, 0) // cap removed
	if e.uploadBlocked {
		t.Fatal("removing the cap should release the pause")
	}
	if allow, disallow := f.counts(); allow != 1 || disallow != 1 {
		t.Fatalf("got allow=%d disallow=%d; want allow=1 disallow=1", allow, disallow)
	}
}

// TestApplySeedRatioIsIndependentOfIdleEviction is the regression guard for the
// feature's core promise (issue #13): reaching the seed ratio must only pause
// uploading, never drop the torrent or purge its cache, so the ratio cap and the
// idle/expiration timeout stay independent knobs.
func TestApplySeedRatioIsIndependentOfIdleEviction(t *testing.T) {
	m := &manager{engines: map[string]*engine{}}
	e := &engine{infoHash: "deadbeef"}
	m.engines[e.infoHash] = e
	f := &fakeToggler{}

	e.applySeedRatio(f, 10_000, 100, 1.0)

	if !e.uploadBlocked {
		t.Fatal("expected upload paused at ratio 100 with cap 1.0")
	}
	if _, ok := m.engines[e.infoHash]; !ok {
		t.Fatal("hitting the seed ratio must not remove the torrent; that is evictIdle's job")
	}
	if _, disallow := f.counts(); disallow != 1 {
		t.Fatalf("expected exactly one DisallowDataUpload, got %d", disallow)
	}
}
