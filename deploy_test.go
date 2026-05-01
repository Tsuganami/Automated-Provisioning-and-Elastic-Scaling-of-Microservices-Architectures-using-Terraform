package main

import (
	"errors"
	"strings"
	"testing"
)

func TestSlicesEqual_Identical(t *testing.T) {
	a := []string{"a", "b", "c"}
	b := []string{"a", "b", "c"}
	if !slicesEqual(a, b) {
		t.Fatal("expected slices to compare equal")
	}
}

func TestSlicesEqual_Different(t *testing.T) {
	a := []string{"a", "b"}
	b := []string{"a", "b", "c"}
	if slicesEqual(a, b) {
		t.Fatal("expected slices to compare unequal")
	}
}

func TestSlicesEqual_SameLengthDifferentContent(t *testing.T) {
	if slicesEqual([]string{"a", "b", "c"}, []string{"a", "x", "c"}) {
		t.Fatal("expected unequal")
	}
}

func TestIsStartupRelatedError_Nil(t *testing.T) {
	if isStartupRelatedError(nil) {
		t.Fatal("nil error must not be classified as startup-related")
	}
}

func TestIsStartupRelatedError_ConnectionRefused(t *testing.T) {
	err := errors.New("dial tcp 10.0.0.1:8080: connection refused")
	if !isStartupRelatedError(err) {
		t.Fatal("connection refused must be classified as startup-related")
	}
}

func TestIsStartupRelatedError_OtherFailure(t *testing.T) {
	err := errors.New("HTTP 500 internal server error")
	if isStartupRelatedError(err) {
		t.Fatal("HTTP 500 should not be a startup-related error")
	}
}

func TestFormatReachabilityError_LongMessageTruncated(t *testing.T) {
	long := strings.Repeat("x", 300)
	out := formatReachabilityError(errors.New(long))

	if !strings.HasPrefix(out, "Unable to reach /metrics endpoint (") {
		t.Fatalf("missing wrapper prefix: %q", out)
	}
	if !strings.HasSuffix(out, "...)") {
		t.Fatalf("expected truncation marker, got %q", out)
	}
	if len(out) >= len(long) {
		t.Fatalf("output not shorter than input (in=%d out=%d)", len(long), len(out))
	}
}

func TestIsKnownInstance_Found(t *testing.T) {
	ips := []string{"10.0.1.1", "10.0.1.2", "10.0.1.3"}
	if !isKnownInstance("10.0.1.2", &ips) {
		t.Fatal("expected 10.0.1.2 to be known")
	}
}

func TestIsKnownInstance_NotFound(t *testing.T) {
	ips := []string{"10.0.1.1", "10.0.1.2"}
	if isKnownInstance("10.0.2.5", &ips) {
		t.Fatal("expected 10.0.2.5 to be unknown")
	}
}

func TestRecordHealthCheckFailure_Increments(t *testing.T) {
	healthCheckFailuresMu.Lock()
	healthCheckFailures["10.0.1.5"] = 3
	healthCheckFailuresMu.Unlock()

	recordHealthCheckFailure("10.0.1.5")

	healthCheckFailuresMu.Lock()
	got := healthCheckFailures["10.0.1.5"]
	healthCheckFailuresMu.Unlock()
	if got != 4 {
		t.Fatalf("got %d want 4", got)
	}
}

func TestRecordSuccessfulHealthCheck_Resets(t *testing.T) {
	healthCheckFailuresMu.Lock()
	healthCheckFailures["10.0.1.3"] = 5
	healthCheckFailuresMu.Unlock()

	recordSuccessfulHealthCheck("10.0.1.3")

	healthCheckFailuresMu.Lock()
	got := healthCheckFailures["10.0.1.3"]
	healthCheckFailuresMu.Unlock()
	if got != 0 {
		t.Fatalf("got %d want 0", got)
	}
}

func TestAddRequestCount_Accumulates(t *testing.T) {
	currentRequestCountsMu.Lock()
	currentRequestCounts["10.0.2.1"] = 150
	currentRequestCountsMu.Unlock()

	addRequestCount("10.0.2.1", 50)

	currentRequestCountsMu.Lock()
	got := currentRequestCounts["10.0.2.1"]
	currentRequestCountsMu.Unlock()
	if got != 200 {
		t.Fatalf("got %v want 200", got)
	}
}

func TestValidateIPFormat_Invalid(t *testing.T) {
	if validateIPFormat("999.999.999.999") {
		t.Fatal("999.999.999.999 must be rejected")
	}
}

func TestValidateIPFormat_Valid(t *testing.T) {
	if !validateIPFormat("10.0.1.1") {
		t.Fatal("10.0.1.1 must be accepted")
	}
}

func TestShouldScaleUp_ThresholdBreach(t *testing.T) {
	if !shouldScaleUp(5000, 3000, 2) {
		t.Fatal("expected scale-up decision when avg > threshold and instances < max")
	}
}

func TestShouldScaleUp_BelowThreshold(t *testing.T) {
	if shouldScaleUp(1000, 3000, 2) {
		t.Fatal("must not scale up below threshold")
	}
}

func TestShouldScaleUp_AtMax(t *testing.T) {
	if shouldScaleUp(99999, 1, MaxInstances) {
		t.Fatal("must not scale up beyond MaxInstances")
	}
}
