package main

import (
	"os"
	"sync"
	"testing"
)

func resetScalerState(t *testing.T) {
	t.Helper()
	monitoredIPsMu.Lock()
	monitoredIPs = nil
	monitoredIPsMu.Unlock()

	for k := range previousRequestCounts {
		delete(previousRequestCounts, k)
	}
	for k := range highCPUSamples {
		delete(highCPUSamples, k)
	}
	currentInstances = 0
}

func TestParseBoolEnv_True(t *testing.T) {
	t.Setenv("PARSE_BOOL", "true")
	if !parseBoolEnv("PARSE_BOOL") {
		t.Fatal("expected true")
	}
}

func TestParseBoolEnv_False(t *testing.T) {
	t.Setenv("TEST_BOOL_VAR", "false")
	if parseBoolEnv("TEST_BOOL_VAR") {
		t.Fatal("expected false")
	}
}

func TestParseBoolEnv_Unset(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_XYZ")
	if parseBoolEnv("DEFINITELY_NOT_SET_XYZ") {
		t.Fatal("unset variable should be false")
	}
}

func TestSetMonitoredIPs_RoundTrip(t *testing.T) {
	resetScalerState(t)
	in := []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"}
	setMonitoredIPs(in)

	got := getMonitoredIPsSnapshot()
	if len(got) != len(in) {
		t.Fatalf("len=%d want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("idx %d: got %q want %q", i, got[i], in[i])
		}
	}
}

func TestSetMonitoredIPs_CleansUpRemovedState(t *testing.T) {
	resetScalerState(t)
	setMonitoredIPs([]string{"10.0.1.1", "10.0.1.2"})
	previousRequestCounts["10.0.1.2"] = 200.0

	setMonitoredIPs([]string{"10.0.1.1"})

	if _, ok := previousRequestCounts["10.0.1.2"]; ok {
		t.Fatal("previousRequestCounts[10.0.1.2] should have been removed")
	}
}

func TestGetMonitoredIPsSnapshot_IsCopy(t *testing.T) {
	resetScalerState(t)
	setMonitoredIPs([]string{"10.0.1.1", "10.0.1.2"})

	snap1 := getMonitoredIPsSnapshot()
	snap1[0] = "modified"

	snap2 := getMonitoredIPsSnapshot()
	if snap2[0] != "10.0.1.1" {
		t.Fatalf("internal state leaked: snap2[0]=%q", snap2[0])
	}
}

func TestGetMonitoredIPsSnapshot_ConcurrentRead(t *testing.T) {
	resetScalerState(t)
	setMonitoredIPs([]string{"10.0.1.1", "10.0.1.2", "10.0.1.3"})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := getMonitoredIPsSnapshot()
			if len(s) != 3 {
				t.Errorf("snapshot len=%d want 3", len(s))
			}
		}()
	}
	wg.Wait()
}

func TestSetMonitoredIPs_ConcurrentWrite(t *testing.T) {
	resetScalerState(t)
	lists := [][]string{
		{"10.0.1.1"},
		{"10.0.1.1", "10.0.1.2"},
		{"10.0.2.1", "10.0.2.2", "10.0.2.3"},
		{"10.0.3.1"},
		{},
	}
	var wg sync.WaitGroup
	for _, l := range lists {
		l := l
		wg.Add(1)
		go func() {
			defer wg.Done()
			setMonitoredIPs(l)
			_ = getMonitoredIPsSnapshot()
		}()
	}
	wg.Wait()

	snap := getMonitoredIPsSnapshot()
	if len(snap) != currentInstances {
		t.Fatalf("inconsistent: snapshot=%d currentInstances=%d", len(snap), currentInstances)
	}
}

func TestSetMonitoredIPs_Empty(t *testing.T) {
	resetScalerState(t)
	setMonitoredIPs([]string{})
	if got := getMonitoredIPsSnapshot(); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
	if currentInstances != 0 {
		t.Fatalf("currentInstances=%d want 0", currentInstances)
	}
}
