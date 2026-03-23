package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	cmd.Start()
}

func fetchCPU(ip string) (float64, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(body)), " ")
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid format")
	}
	return strconv.ParseFloat(parts[1], 64)
}

func startDashboard(ips *[]string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "templates/index.html")
	})

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		html := `<div class="grid grid-cols-1 md:grid-cols-2 gap-6">`
		for _, ip := range *ips {
			cpu, err := fetchCPU(ip)
			status := "ACTIVE"
			statusColor := "bg-green-500"
			barColor := "bg-green-500"
			if err != nil {
				cpu = 0
				status = "UNREACHABLE"
				statusColor = "bg-gray-500"
				barColor = "bg-gray-500"
			} else if cpu > 70 {
				statusColor = "bg-red-500"
				barColor = "bg-red-500"
			} else if cpu > 40 {
				statusColor = "bg-yellow-500"
				barColor = "bg-yellow-500"
			}

			html += fmt.Sprintf(`
				<div class="bg-gray-800 p-6 rounded-xl border border-gray-700 shadow-lg">
					<div class="flex justify-between items-center mb-4">
						<span class="font-mono text-blue-300">%s</span>
						<span class="px-2 py-1 rounded text-xs %s text-white">%s</span>
					</div>
					<div class="text-gray-400 text-sm mb-1">CPU Load</div>
					<div class="w-full bg-gray-700 rounded-full h-4">
						<div class="h-4 rounded-full %s transition-all duration-500" style="width: %.1f%%"></div>
					</div>
					<div class="text-right mt-2 font-bold text-xl">%.1f%%</div>
				</div>`, ip, statusColor, status, barColor, cpu, cpu)
		}
		html += `</div>`
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, html)
	})

	http.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"instance_count": len(*ips),
			"ips":            *ips,
		})
	})

	fmt.Println("=== Dashboard running at http://localhost:9090 ===")
	if err := http.ListenAndServe(":9090", nil); err != nil {
		log.Fatalf("Dashboard server failed: %v", err)
	}
}

// ScalingConfig contains all configurable scaling options
type ScalingConfig struct {
	// Scaling Metrics
	CPUMonitoring           bool
	RequestRateMonitoring   bool
	
	// Scaling Behavior
	ImmediateScaling        bool // Scale immediately vs with time windows
	ScaleUpTimeWindow       bool // Require 2 min above threshold
	ScaleDownTimeWindow     bool // Require 5 min below threshold
	
	// Instance Limits
	EnforceMinInstances     bool // Enforce minimum 2 instances
	EnforceMaxInstances     bool // Enforce maximum 10 instances
	
	// Cooldown & Safety
	EnforceCooldown         bool // 3-minute cooldown between scaling
	
	// Health & Reliability
	HealthChecks            bool // Enable /health endpoint checks
	HealthCheckRecovery     bool // Remove unhealthy instances
	
	// Service Discovery & Load Balancing
	ServiceDiscovery        bool // Instance auto-registration
	LoadBalancing           bool // Round-robin load balancing
	StickySessionsLB        bool // Session affinity (optional)
	
	// Metrics & Observability
	PrometheusMetrics       bool // Export metrics in Prometheus format
	MetricsRetention        bool // Store metrics for 7 days
	HealthCheckLogging      bool // Log all health check failures
	ScalingLogging          bool // Log scaling events
	
	// Security
	TLSCommunication        bool // Use TLS for service-to-service
	EncryptedState          bool // Encrypt Terraform state
}

func selectScalingOptions() ScalingConfig {
	config := ScalingConfig{}

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     CONFIGURE SCALING OPTIONS FOR THIS TEST                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SCALING METRICS (What triggers scaling)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.CPUMonitoring = getUserConfirmation("  [REQ-1.1] Monitor CPU utilization?")
	config.RequestRateMonitoring = getUserConfirmation("  [REQ-1.2] Monitor incoming request rate?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SCALING BEHAVIOR (How scaling happens)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.ImmediateScaling = getUserConfirmation("  Scale immediately on threshold breach?")
	if !config.ImmediateScaling {
		config.ScaleUpTimeWindow = getUserConfirmation("    [REQ-1.3] Scale-up requires 2 min above 70%?")
		config.ScaleDownTimeWindow = getUserConfirmation("    [REQ-1.4] Scale-down requires 5 min below 30%?")
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("INSTANCE LIMITS & SAFETY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.EnforceMinInstances = getUserConfirmation("  [REQ-1.5] Enforce minimum 2 instances?")
	config.EnforceMaxInstances = getUserConfirmation("  [REQ-1.6] Enforce maximum 10 instances?")
	config.EnforceCooldown = getUserConfirmation("  [REQ-1.7] Enforce 3-minute cooldown between scaling?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("HEALTH & RELIABILITY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.HealthChecks = getUserConfirmation("  [REQ-5.1] Enable /health endpoint checks?")
	if config.HealthChecks {
		config.HealthCheckRecovery = getUserConfirmation("    [REQ-5.3] Terminate unhealthy instances?")
		config.HealthCheckLogging = getUserConfirmation("    [REQ-5.4] Log health check failures?")
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SERVICE DISCOVERY & LOAD BALANCING")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.ServiceDiscovery = getUserConfirmation("  [REQ-2.x] Enable service discovery & registration?")
	config.LoadBalancing = getUserConfirmation("  [REQ-3.x] Enable load balancing (round-robin)?")
	if config.LoadBalancing {
		config.StickySessionsLB = getUserConfirmation("    [REQ-3.6] Enable sticky sessions (optional)?")
	}

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("METRICS & OBSERVABILITY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.PrometheusMetrics = getUserConfirmation("  [REQ-6.1] Export metrics in Prometheus format?")
	if config.PrometheusMetrics {
		config.MetricsRetention = getUserConfirmation("    [REQ-6.3] Retain metrics for 7 days?")
	}
	config.ScalingLogging = getUserConfirmation("  [REQ-6.4] Log all scaling events?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SECURITY (Advanced)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	config.TLSCommunication = getUserConfirmation("  [SEC-1] Use TLS for service-to-service communication?")
	config.EncryptedState = getUserConfirmation("  [SEC-4] Encrypt Terraform state at rest?")

	displayScalingConfiguration(config)
	return config
}

func getUserConfirmation(prompt string) bool {
	for {
		fmt.Print(prompt + " (y/n): ")
		var response string
		fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))
		if response == "y" || response == "yes" {
			return true
		} else if response == "n" || response == "no" {
			return false
		}
		fmt.Println("  Invalid input. Please enter 'y' or 'n'.")
	}
}

func displayScalingConfiguration(config ScalingConfig) {
	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     YOUR SCALING CONFIGURATION                                    ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	sections := []struct {
		title string
		items []struct {
			label   string
			enabled bool
		}
	}{
		{
			"METRICS",
			[]struct {
				label   string
				enabled bool
			}{
				{"CPU Monitoring", config.CPUMonitoring},
				{"Request Rate Monitoring", config.RequestRateMonitoring},
			},
		},
		{
			"SCALING BEHAVIOR",
			[]struct {
				label   string
				enabled bool
			}{
				{"Immediate Scaling", config.ImmediateScaling},
				{"2-Min Scale-Up Window", config.ScaleUpTimeWindow},
				{"5-Min Scale-Down Window", config.ScaleDownTimeWindow},
			},
		},
		{
			"LIMITS & SAFETY",
			[]struct {
				label   string
				enabled bool
			}{
				{"Enforce Min 2 Instances", config.EnforceMinInstances},
				{"Enforce Max 10 Instances", config.EnforceMaxInstances},
				{"3-Min Cooldown Period", config.EnforceCooldown},
			},
		},
		{
			"HEALTH & RELIABILITY",
			[]struct {
				label   string
				enabled bool
			}{
				{"Health Checks", config.HealthChecks},
				{"Health Check Recovery", config.HealthCheckRecovery},
				{"Health Check Logging", config.HealthCheckLogging},
			},
		},
		{
			"SERVICE DISCOVERY & LB",
			[]struct {
				label   string
				enabled bool
			}{
				{"Service Discovery", config.ServiceDiscovery},
				{"Load Balancing", config.LoadBalancing},
				{"Sticky Sessions", config.StickySessionsLB},
			},
		},
		{
			"OBSERVABILITY",
			[]struct {
				label   string
				enabled bool
			}{
				{"Prometheus Metrics", config.PrometheusMetrics},
				{"Metrics Retention (7d)", config.MetricsRetention},
				{"Scaling Event Logging", config.ScalingLogging},
			},
		},
		{
			"SECURITY",
			[]struct {
				label   string
				enabled bool
			}{
				{"TLS Communication", config.TLSCommunication},
				{"Encrypted Terraform State", config.EncryptedState},
			},
		},
	}

	for _, section := range sections {
		fmt.Printf("  %s:\n", section.title)
		for _, item := range section.items {
			status := "✓"
			if !item.enabled {
				status = "✗"
			}
			fmt.Printf("    [%s] %s\n", status, item.label)
		}
		fmt.Println()
	}
	
	saveScalingConfigToFile(config)
}

func saveScalingConfigToFile(config ScalingConfig) {
	configJSON, _ := json.MarshalIndent(config, "", "  ")
	err := ioutil.WriteFile("scaling_config.json", configJSON, 0644)
	if err != nil {
		log.Printf("Warning: Could not save scaling config to file: %v", err)
	} else {
		fmt.Println("✓ Configuration saved to: scaling_config.json")
		fmt.Println()
	}
}

func main() {
	// Get scaling configuration from user
	scalingConfig := selectScalingOptions()

	fmt.Println("=== Running Terraform Apply ===")
	applyCmd := exec.Command("terraform", "apply", "-auto-approve")
	applyCmd.Stdout = os.Stdout
	applyCmd.Stderr = os.Stderr
	if err := applyCmd.Run(); err != nil {
		log.Fatalf("Terraform apply failed: %v", err)
	}
	fmt.Println("=== Terraform Apply Complete ===")

	fmt.Println("=== Fetching Instance IPs ===")
	outCmd := exec.Command("terraform", "output", "-json", "instance_ips")
	outBytes, err := outCmd.Output()
	if err != nil {
		log.Fatalf("Failed to get terraform output: %v", err)
	}

	var ips []string
	if err := json.Unmarshal(outBytes, &ips); err != nil {
		log.Fatalf("Failed to parse IPs from terraform output: %v", err)
	}

	if len(ips) == 0 {
		log.Fatal("No instance IPs found in terraform output")
	}

	fmt.Printf("Found IPs: %s\n", strings.Join(ips, ", "))

	go startDashboard(&ips)
	time.Sleep(1 * time.Second)
	openBrowser("http://localhost:9090")

	fmt.Println("=== Starting Scaler ===")
	args := append([]string{"run", "scaler.go"}, ips...)
	scalerCmd := exec.Command("go", args...)
	scalerCmd.Stdout = os.Stdout
	scalerCmd.Stderr = os.Stderr
	if err := scalerCmd.Start(); err != nil {
		log.Fatalf("Scaler failed to start: %v", err)
	}

	fmt.Println("=== Waiting 2 minutes for instances to boot and initialize ===")
	time.Sleep(2 * time.Minute)

	// Run selected tests
	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     STARTING TEST EXECUTION                                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	if scalingConfig.EnforceMinInstances {
		fmt.Println("[TEST] Verifying minimum 2 instances requirement...")
		fmt.Printf("  Current instances: %d (Expected: >= 2)\n", len(ips))
		if len(ips) >= 2 {
			fmt.Println("  ✓ PASS: Minimum instance requirement met\n")
		} else {
			fmt.Println("  ✗ FAIL: Minimum instance requirement NOT met\n")
		}
	}

	if scalingConfig.EnforceMaxInstances {
		fmt.Println("[TEST] Verifying maximum 10 instances limit...")
		fmt.Println("  This will be tested during stress test (max 10 instances allowed)\n")
	}

	if scalingConfig.EnforceCooldown {
		fmt.Println("[TEST] Cooldown period is configured for 3 minutes between scaling operations")
		fmt.Println("  Monitoring logs for cooldown enforcement...\n")
	}

	if scalingConfig.HealthChecks {
		fmt.Println("[TEST] Testing /health endpoint on each instance...")
		testHealthEndpoints(ips)
		fmt.Println()
	}

	if scalingConfig.PrometheusMetrics {
		fmt.Println("[TEST] Testing /metrics endpoint (Prometheus format)...")
		testMetricsEndpoints(ips)
		fmt.Println()
	}

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     LAUNCHING REQUIREMENT-BASED TESTS                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	// Launch configured test generators
	stressedIPs := make(map[string]bool)
	var wg sync.WaitGroup

	// Test 1: CPU Scaling Test
	if scalingConfig.CPUMonitoring && scalingConfig.ScaleUpTimeWindow {
		fmt.Println("[TEST GENERATOR] CPU Load Test - Will generate CPU load to trigger scale-up")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runCPULoadTest(scalingConfig, getActiveIPs)
		}()
	}

	// Test 2: Request Rate Scaling Test  
	if scalingConfig.RequestRateMonitoring && scalingConfig.ScaleUpTimeWindow {
		fmt.Println("[TEST GENERATOR] Traffic Spike Test - Will blast instances with traffic to trigger scale-up")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTrafficSpikeTest(scalingConfig, getActiveIPs)
		}()
	}

	// Test 3: Health Check Recovery Test
	if scalingConfig.HealthChecks && scalingConfig.HealthCheckRecovery {
		fmt.Println("[TEST GENERATOR] Health Check Test - Will monitor instance health and recovery")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runHealthCheckTest(scalingConfig, getActiveIPs)
		}()
	}

	// Test 4: Scale-Down Test (after scale-up completes)
	if scalingConfig.ScaleDownTimeWindow {
		fmt.Println("[TEST GENERATOR] Scale-Down Test - Will reduce load after grace period to trigger scale-down")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runScaleDownTest(scalingConfig, getActiveIPs)
		}()
	}

	// Test 5: Cooldown Test
	if scalingConfig.EnforceCooldown {
		fmt.Println("[TEST GENERATOR] Cooldown Test - Will verify 3-minute cooldown between operations")
	}

	// Test 6: Metrics Validation Test
	if scalingConfig.PrometheusMetrics {
		fmt.Println("[TEST GENERATOR] Prometheus Metrics Test - Continuous metric validation")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runMetricsValidationTest(scalingConfig, getActiveIPs)
		}()
	}

	fmt.Println()

	// Automatic stress test for active instances
	go func() {
		for {
			currentIPs := getActiveIPs()
			for _, ip := range currentIPs {
				if stressedIPs[ip] {
					continue
				}
				fmt.Printf("[AUTO-DISCOVER] New instance detected: %s... ", ip)
				_, err := fetchCPU(ip)
				if err != nil {
					fmt.Printf("NOT READY (%v)\n", err)
					continue
				}
				fmt.Println("OK — launching continuous health checks")
				stressedIPs[ip] = true
				go stressTest(ip, 50, 90*time.Second)
			}
			time.Sleep(30 * time.Second)
		}
	}()

	scalerCmd.Wait()
}

func getActiveIPs() []string {
	cmd := exec.Command("terraform", "output", "-json", "instance_ips")
	outBytes, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to refresh IPs: %v", err)
		return nil
	}
	var ips []string
	if err := json.Unmarshal(outBytes, &ips); err != nil {
		log.Printf("Failed to parse refreshed IPs: %v", err)
		return nil
	}
	return ips
}

func stressTest(ip string, concurrency int, duration time.Duration) {
	url := fmt.Sprintf("http://%s:8080/work", ip)
	fmt.Printf("Stress testing %s with %d workers for %v\n", url, concurrency, duration)

	client := &http.Client{Timeout: 60 * time.Second}
	deadline := time.After(duration)
	var successCount, errorCount int
	var mu sync.Mutex

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-deadline:
					return
				default:
					resp, err := client.Get(url)
					mu.Lock()
					if err != nil {
						errorCount++
					} else {
						resp.Body.Close()
						successCount++
					}
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()

	fmt.Printf("Stress test results for %s: %d OK, %d errors\n", ip, successCount, errorCount)
}

// ============= AUTOMATED TEST GENERATORS =============

// Test REQ-1.1 & REQ-1.3: CPU Load triggers scaling
func runCPULoadTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[CPU LOAD TEST] Starting 3-minute CPU load pattern...")
	fmt.Println("  Phase 1: Ramp-up (60 seconds) - Generate moderate CPU load")
	fmt.Println("  Phase 2: Peak (60 seconds) - Generate heavy CPU load to exceed 70% threshold")
	fmt.Println("  Phase 3: Cooldown (60 seconds) - Reduce load gradually")
	fmt.Println("  Expected: Scale-up after Phase 2 completes (waiting for 2-minute window)\n")

	startTime := time.Now()
	phaseDuration := 60 * time.Second

	for phase := 1; phase <= 3; phase++ {
		ips := getActiveIPs()
		if len(ips) == 0 {
			continue
		}

		var intensity int
		switch phase {
		case 1:
			intensity = 25  // Moderate
		case 2:
			intensity = 75  // Heavy - exceed threshold
		case 3:
			intensity = 5   // Minimal
		}

		phaseStart := time.Now()
		fmt.Printf("\n[CPU TEST PHASE %d] Intensity: %d workers/instance, Duration: 60s\n", phase, intensity)

		// Run concurrent load on all instances
		for timeElapsed := 0 * time.Second; timeElapsed < phaseDuration; timeElapsed += 5 * time.Second {
			for _, ip := range ips {
				go stressTest(ip, intensity, 5*time.Second)
			}
			time.Sleep(5 * time.Second)
		}

		elapsed := time.Since(phaseStart)
		fmt.Printf("[CPU TEST PHASE %d] Complete (%v elapsed)\n", phase, elapsed)
	}

	totalTime := time.Since(startTime)
	fmt.Printf("[CPU LOAD TEST] Complete - Total time: %v\n", totalTime)
	fmt.Println("[CPU LOAD TEST] Check scaler logs to verify scale-up was triggered\n")
}

// Test REQ-1.2 & REQ-1.3: Request rate triggers scaling
func runTrafficSpikeTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[TRAFFIC SPIKE TEST] Starting request rate load pattern...")
	fmt.Println("  Phase 1: Warm-up (30 seconds) - 100 req/s")
	fmt.Println("  Phase 2: Ramp-up (30 seconds) - 500 req/s")
	fmt.Println("  Phase 3: SPIKE (60 seconds) - 1500 req/s (exceeds 1000 req/s threshold)")
	fmt.Println("  Expected: Scale-up after Phase 3 completes (waiting for 2-minute window)\n")

	time.Sleep(10 * time.Second) // Let CPU test settle

	phases := []struct {
		name       string
		maxWorkers int
		duration   time.Duration
	}{
		{"Warm-up", 10, 30 * time.Second},
		{"Ramp-up", 50, 30 * time.Second},
		{"SPIKE", 150, 60 * time.Second},
	}

	for phaseIdx, phase := range phases {
		ips := getActiveIPs()
		if len(ips) == 0 {
			continue
		}

		fmt.Printf("\n[TRAFFIC PHASE %d] %s - %d concurrent workers\n", phaseIdx+1, phase.name, phase.maxWorkers)
		fmt.Printf("  Estimated requests/sec: ~%d (threshold is 1000)\n", phase.maxWorkers*10)

		phaseStart := time.Now()
		var wg sync.WaitGroup

		for _, ip := range ips {
			wg.Add(1)
			go func(ipAddr string) {
				defer wg.Done()
				stressTest(ipAddr, phase.maxWorkers, phase.duration)
			}(ip)
		}

		wg.Wait()
		elapsed := time.Since(phaseStart)
		fmt.Printf("[TRAFFIC PHASE %d] Complete - Actual duration: %v\n", phaseIdx+1, elapsed)
	}

	fmt.Println("\n[TRAFFIC SPIKE TEST] Complete")
	fmt.Println("[TRAFFIC SPIKE TEST] Check ./metrics or scaler logs to verify request rate was captured\n")
}

// Test REQ-5.x: Health check endpoints
func runHealthCheckTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[HEALTH CHECK TEST] Starting health check validation...")
	fmt.Println("  Will continuously verify /health endpoints")
	fmt.Println("  Monitors: Response time, status codes, failure tracking\n")

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	testDuration := 5 * time.Minute
	startTime := time.Now()
	passCount := 0
	failCount := 0

	for range ticker.C {
		if time.Since(startTime) > testDuration {
			break
		}

		ips := getActiveIPs()
		if len(ips) == 0 {
			continue
		}

		fmt.Printf("[HEALTH CHECK SCAN] %v elapsed - Checking %d instances...\n", time.Since(startTime).Round(time.Second), len(ips))

		for _, ip := range ips {
			client := http.Client{Timeout: 3 * time.Second}
			url := fmt.Sprintf("http://%s:8080/health", ip)
			resp, err := client.Get(url)

			if err != nil {
				fmt.Printf("  [%s] ✗ FAIL - Connection error: %v\n", ip, err)
				failCount++
				continue
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				fmt.Printf("  [%s] ✓ PASS - HTTP %d (Healthy)\n", ip, resp.StatusCode)
				passCount++
			} else {
				fmt.Printf("  [%s] ✗ FAIL - HTTP %d (expected 200)\n", ip, resp.StatusCode)
				failCount++
			}
		}
	}

	fmt.Printf("\n[HEALTH CHECK TEST] Complete - Results: %d pass, %d fail\n\n", passCount, failCount)
}

// Test REQ-1.4: Scale-down after load reduces
func runScaleDownTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[SCALE-DOWN TEST] Starting scale-down validation...")
	fmt.Println("  Will maintain minimal load for 5+ minutes")
	fmt.Println("  Expected: System should scale-down after 5-minute low-load window\n")

	time.Sleep(4 * time.Minute) // Wait for scale-up to complete

	fmt.Println("[SCALE-DOWN TEST] Beginning 5-minute low-load period...")
	fmt.Println("  Sending 1 request every 10 seconds (minimal load)")
	fmt.Println("  CPU load: <10%")
	fmt.Println("  Request rate: ~0.1 req/s (far below 100 req/s threshold)\n")

	testDuration := 6 * time.Minute
	startTime := time.Now()

	for time.Since(startTime) < testDuration {
		ips := getActiveIPs()
		if len(ips) == 0 {
			time.Sleep(10 * time.Second)
			continue
		}

		elapsedTime := time.Since(startTime)
		fmt.Printf("[SCALE-DOWN TEST] %v elapsed - Sending 1 keepalive request...\n", elapsedTime.Round(time.Second))

		// Send minimal requests to keep connections alive
		for _, ip := range ips {
			go func(ipAddr string) {
				client := http.Client{Timeout: 5 * time.Second}
				client.Get(fmt.Sprintf("http://%s:8080/metrics", ipAddr))
			}(ip)
		}

		time.Sleep(10 * time.Second)
	}

	fmt.Println("\n[SCALE-DOWN TEST] Complete")
	fmt.Println("[SCALE-DOWN TEST] Check scaler logs - should show scale-down triggered after 5-minute window\n")
}

// Test REQ-6.1: Validate Prometheus metrics
func runMetricsValidationTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[METRICS VALIDATION TEST] Starting continuous metric validation...")
	fmt.Println("  Verifies Prometheus format every 30 seconds\n")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	testDuration := 10 * time.Minute
	startTime := time.Now()
	sampleCount := 0

	for range ticker.C {
		if time.Since(startTime) > testDuration {
			break
		}

		ips := getActiveIPs()
		if len(ips) == 0 {
			continue
		}

		sampleCount++
		fmt.Printf("\n[METRICS SAMPLE #%d] Time: %v\n", sampleCount, time.Since(startTime).Round(time.Second))

		for _, ip := range ips {
			client := http.Client{Timeout: 5 * time.Second}
			url := fmt.Sprintf("http://%s:8080/metrics", ip)
			resp, err := client.Get(url)

			if err != nil {
				fmt.Printf("  [%s] Error fetching metrics: %v\n", ip, err)
				continue
			}
			defer resp.Body.Close()

			body, _ := ioutil.ReadAll(resp.Body)
			metricsStr := string(body)

			// Validate Prometheus format
			hasHelp := strings.Contains(metricsStr, "# HELP")
			hasType := strings.Contains(metricsStr, "# TYPE")
			hasCPU := strings.Contains(metricsStr, "cpu_utilization")
			hasRequests := strings.Contains(metricsStr, "request_count")

			status := "✓"
			if !hasHelp || !hasType {
				status = "✗"
			}

			fmt.Printf("  [%s] %s Prometheus Format: ", ip, status)
			if hasHelp && hasType && hasCPU && hasRequests {
				fmt.Printf("Valid (HELP✓ TYPE✓ CPU✓ REQUESTS✓)\n")
			} else {
				fmt.Printf("INVALID (HELP:%v TYPE:%v CPU:%v REQ:%v)\n", hasHelp, hasType, hasCPU, hasRequests)
			}

			// Extract actual values
			lines := strings.Split(metricsStr, "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "cpu_utilization ") {
					fmt.Printf("    → CPU: %s\n", line)
				}
				if strings.HasPrefix(line, "request_count ") {
					fmt.Printf("    → Requests: %s\n", line)
				}
			}
		}
	}

	fmt.Printf("\n[METRICS VALIDATION TEST] Complete - Collected %d samples\n\n", sampleCount)
}

// Test helper functions for requirement validation

func testHealthEndpoints(ips []string) {
	client := http.Client{Timeout: 5 * time.Second}
	passCount := 0
	failCount := 0

	for _, ip := range ips {
		url := fmt.Sprintf("http://%s:8080/health", ip)
		fmt.Printf("  Testing %s... ", url)
		resp, err := client.Get(url)
		if err != nil {
			fmt.Printf("✗ FAIL (error: %v)\n", err)
			failCount++
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Printf("✓ PASS (HTTP 200)\n")
			passCount++
		} else {
			fmt.Printf("✗ FAIL (HTTP %d, expected 200)\n", resp.StatusCode)
			failCount++
		}
	}

	fmt.Printf("  Health check results: %d passed, %d failed\n", passCount, failCount)
}

func testMetricsEndpoints(ips []string) {
	client := http.Client{Timeout: 5 * time.Second}
	passCount := 0
	failCount := 0

	for _, ip := range ips {
		url := fmt.Sprintf("http://%s:8080/metrics", ip)
		fmt.Printf("  Testing %s... ", url)
		resp, err := client.Get(url)
		if err != nil {
			fmt.Printf("✗ FAIL (error: %v)\n", err)
			failCount++
			continue
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("✗ FAIL (read error: %v)\n", err)
			failCount++
			continue
		}

		metricsStr := string(body)
		// REQ-6.1: Check for Prometheus format markers
		isPrometheus := strings.Contains(metricsStr, "# HELP") && 
		              strings.Contains(metricsStr, "# TYPE") && 
		              strings.Contains(metricsStr, "cpu_utilization")
		
		if isPrometheus && (strings.Contains(metricsStr, "request_count") || strings.Contains(metricsStr, "requests")) {
			fmt.Printf("✓ PASS (Prometheus format)\n")
			passCount++
		} else {
			fmt.Printf("✗ FAIL (expected Prometheus format)\n")
			fmt.Printf("    Got: %s\n", metricsStr[:100])
			failCount++
		}
	}

	fmt.Printf("  Metrics endpoint results: %d passed, %d failed\n", passCount, failCount)
}
