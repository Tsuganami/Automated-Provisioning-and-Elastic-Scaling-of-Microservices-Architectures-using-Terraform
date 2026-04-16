package main

import (
	"errors"
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

const instanceStartupGracePeriod = 4 * time.Minute

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

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cpu_utilization ") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("invalid cpu metric format")
			}
			return strconv.ParseFloat(parts[1], 64)
		}
	}
	return 0, fmt.Errorf("cpu metric not found")
}

func fetchMetrics(ip string) (float64, float64, error) {
	client := http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, err
	}

	var cpuValue float64
	var requestCount float64
	var foundCPU bool
	var foundRequests bool
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cpu_utilization ") || strings.HasPrefix(line, "request_count ") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			value, parseErr := strconv.ParseFloat(parts[1], 64)
			if parseErr != nil {
				continue
			}
			switch parts[0] {
			case "cpu_utilization":
				cpuValue = value
				foundCPU = true
			case "request_count":
				requestCount = value
				foundRequests = true
			}
		}
	}

	if !foundCPU && !foundRequests {
		return 0, 0, fmt.Errorf("metrics not found")
	}

	return cpuValue, requestCount, nil
}

func isStartupRelatedError(err error) bool {
	if err == nil {
		return false
	}

	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") ||
		strings.Contains(message, "timeout") ||
		strings.Contains(message, "deadline exceeded") ||
		strings.Contains(message, "no route to host") ||
		strings.Contains(message, "i/o timeout")
}

func formatReachabilityError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 180 {
		message = message[:180] + "..."
	}
	return fmt.Sprintf("Unable to reach /metrics endpoint (%s)", message)
}

func checkLBHealth(ip string) error {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", ip))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("lb returned status %d", resp.StatusCode)
	}
	return nil
}

type instanceStatus struct {
	IP           string  `json:"ip"`
	Role         string  `json:"role"`
	CPUPercent   float64 `json:"cpu_percent"`
	RequestCount float64 `json:"request_count"`
	RequestRate  float64 `json:"request_rate"`
	Status       string  `json:"status"`
	Healthy      bool    `json:"healthy"`
	Error        string  `json:"error,omitempty"`
}

func isKnownInstance(ip string, ips *[]string) bool {
	for _, knownIP := range *ips {
		if knownIP == ip {
			return true
		}
	}
	return false
}

func startDashboard(allIPs *[]string, appIPs *[]string, lbIP *string) {
	instanceSeenAt := make(map[string]time.Time)
	var instanceSeenMu sync.Mutex
	var requestRateMu sync.Mutex
	var requestLoadMu sync.RWMutex
	var lastTotalRequests float64
	var lastRequestSampleTime time.Time
	lastRequestByIP := make(map[string]float64)
	lastRequestTimeByIP := make(map[string]time.Time)
	requestLoadRPSByIP := make(map[string]int)
	for _, ip := range *allIPs {
		instanceSeenAt[ip] = time.Now()
	}

	// Keep sending configured synthetic request load continuously (req/s per app instance).
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			knownApps := make(map[string]struct{}, len(*appIPs))
			for _, ip := range *appIPs {
				knownApps[ip] = struct{}{}
			}

			requestLoadMu.Lock()
			for ip := range requestLoadRPSByIP {
				if _, ok := knownApps[ip]; !ok {
					delete(requestLoadRPSByIP, ip)
				}
			}
			currentLoad := make(map[string]int, len(requestLoadRPSByIP))
			for ip, rps := range requestLoadRPSByIP {
				currentLoad[ip] = rps
			}
			requestLoadMu.Unlock()

			for ip, rps := range currentLoad {
				for i := 0; i < rps; i++ {
					go func(targetIP string) {
						resp, err := client.Get(fmt.Sprintf("http://%s:8080/request", targetIP))
						if err != nil {
							log.Printf("[REQUEST LOAD] Failed /request on %s: %v", targetIP, err)
							return
						}
						resp.Body.Close()
					}(ip)
				}
			}
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "templates/index.html")
	})

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		statuses := make([]instanceStatus, 0, len(*allIPs))
		for _, ip := range *allIPs {
			instanceSeenMu.Lock()
			seenAt, exists := instanceSeenAt[ip]
			if !exists {
				seenAt = time.Now()
				instanceSeenAt[ip] = seenAt
			}
			instanceSeenMu.Unlock()

			role := "APP"
			if ip == *lbIP {
				role = "LB-SENDER"
			}
			status := instanceStatus{
				IP:      ip,
				Role:    role,
				Healthy: true,
				Status:  "ACTIVE",
			}
			if role == "LB-SENDER" {
				err := checkLBHealth(ip)
				if err != nil {
					status.Healthy = false
					if time.Since(seenAt) <= instanceStartupGracePeriod && isStartupRelatedError(err) {
						status.Status = "STARTING"
						status.Error = "Load balancer node is starting. Routing will be available shortly."
					} else {
						status.Status = "UNREACHABLE"
						status.Error = fmt.Sprintf("Unable to reach nginx load balancer (%s)", strings.TrimSpace(err.Error()))
					}
				} else {
					status.Status = "LB ACTIVE"
				}
				statuses = append(statuses, status)
				continue
			}

			cpu, requests, err := fetchMetrics(ip)
			if err != nil {
				status.Healthy = false
				if time.Since(seenAt) <= instanceStartupGracePeriod && isStartupRelatedError(err) {
					status.Status = "STARTING"
					status.Error = "Instance is starting. Metrics will appear automatically when the service is ready."
				} else {
					status.Status = "UNREACHABLE"
					status.Error = formatReachabilityError(err)
				}
				statuses = append(statuses, status)
				continue
			} else if cpu > 70 {
				status.Status = "HIGH LOAD"
			} else if cpu > 40 {
				status.Status = "ELEVATED"
			}

			status.CPUPercent = cpu
			status.RequestCount = requests

			requestRateMu.Lock()
			prevRequests, hasPrevRequests := lastRequestByIP[ip]
			prevAt, hasPrevAt := lastRequestTimeByIP[ip]
			if hasPrevRequests && hasPrevAt {
				elapsed := time.Since(prevAt).Seconds()
				if elapsed > 0 {
					delta := requests - prevRequests
					if delta < 0 {
						delta = 0
					}
					status.RequestRate = delta / elapsed
				}
			}
			lastRequestByIP[ip] = requests
			lastRequestTimeByIP[ip] = time.Now()
			requestRateMu.Unlock()

			statuses = append(statuses, status)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"instances": statuses,
		})
	})

	http.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		healthyCount := 0
		unhealthyCount := 0
		var totalCPU float64
		var totalRequests float64
		appHealthyCount := 0
		for _, ip := range *allIPs {
			if ip == *lbIP {
				err := checkLBHealth(ip)
				if err != nil {
					unhealthyCount++
				} else {
					healthyCount++
				}
				continue
			}

			cpu, requests, err := fetchMetrics(ip)
			if err != nil {
				unhealthyCount++
				continue
			}
			healthyCount++
			appHealthyCount++
			totalCPU += cpu
			totalRequests += requests
		}
		averageCPU := 0.0
		if appHealthyCount > 0 {
			averageCPU = totalCPU / float64(appHealthyCount)
		}

		requestRate := 0.0
		requestRateMu.Lock()
		now := time.Now()
		if !lastRequestSampleTime.IsZero() {
			elapsed := now.Sub(lastRequestSampleTime).Seconds()
			if elapsed > 0 {
				delta := totalRequests - lastTotalRequests
				if delta < 0 {
					delta = 0
				}
				requestRate = delta / elapsed
			}
		}
		lastTotalRequests = totalRequests
		lastRequestSampleTime = now
		requestRateMu.Unlock()

		json.NewEncoder(w).Encode(map[string]interface{}{
			"instance_count":  len(*allIPs),
			"healthy_count":   healthyCount,
			"unhealthy_count": unhealthyCount,
			"average_cpu":     averageCPU,
			"total_requests":  totalRequests,
			"request_rate":    requestRate,
		})
	})

	http.HandleFunc("/api/load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ip := strings.TrimSpace(r.URL.Query().Get("ip"))
		if ip == "" {
			http.Error(w, "missing ip", http.StatusBadRequest)
			return
		}

		if !isKnownInstance(ip, appIPs) {
			http.Error(w, "ip is not part of active deployment", http.StatusBadRequest)
			return
		}

		burst := 1
		go func(targetIP string) {
			client := &http.Client{Timeout: 35 * time.Second}
			for i := 0; i < burst; i++ {
				go func() {
					resp, err := client.Get(fmt.Sprintf("http://%s:8080/work", targetIP))
					if err != nil {
						log.Printf("[DASHBOARD LOAD] Failed to hit /work on %s: %v", targetIP, err)
						return
					}
					resp.Body.Close()
				}()
			}
		}(ip)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "accepted",
			"ip":      ip,
			"requests": burst,
		})
	})

	http.HandleFunc("/api/request-load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ip := strings.TrimSpace(r.URL.Query().Get("ip"))
		if ip == "" {
			http.Error(w, "missing ip", http.StatusBadRequest)
			return
		}

		if !isKnownInstance(ip, appIPs) {
			http.Error(w, "ip is not part of app deployment", http.StatusBadRequest)
			return
		}

		const increaseByRPS = 5
		requestLoadMu.Lock()
		requestLoadRPSByIP[ip] += increaseByRPS
		currentRPS := requestLoadRPSByIP[ip]
		requestLoadMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "accepted",
			"ip":          ip,
			"added_rps":   increaseByRPS,
			"current_rps": currentRPS,
		})
	})

	fmt.Println("=== Dashboard running at http://localhost:9090 ===")
	if err := http.ListenAndServe(":9090", nil); err != nil {
		log.Fatalf("Dashboard server failed: %v", err)
	}
}

type ScalingConfig struct {
	CPUMonitoring           bool
	RequestRateMonitoring   bool
	
	ImmediateScaling        bool
	ScaleUpTimeWindow       bool
	ScaleDownTimeWindow     bool
	
	EnforceMinInstances     bool
	EnforceMaxInstances     bool
	
	EnforceCooldown         bool
	
	HealthChecks            bool
	HealthCheckRecovery     bool
	
	ServiceDiscovery        bool
	LoadBalancing           bool
	StickySessionsLB        bool
	
	PrometheusMetrics       bool
	MetricsRetention        bool
	HealthCheckLogging      bool
	ScalingLogging          bool
	
	TLSCommunication        bool
	EncryptedState          bool
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
	config.ImmediateScaling = true
	// config.ScaleUpTimeWindow = getUserConfirmation("    [REQ-1.3] Scale-up requires 2 min above 70%?")
	// config.ScaleDownTimeWindow = getUserConfirmation("    [REQ-1.4] Scale-down requires 5 min below 30%?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("INSTANCE LIMITS & SAFETY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	// config.EnforceMinInstances = getUserConfirmation("  [REQ-1.5] Enforce minimum 2 instances?")
	// config.EnforceMaxInstances = getUserConfirmation("  [REQ-1.6] Enforce maximum 10 instances?")
	// config.EnforceCooldown = getUserConfirmation("  [REQ-1.7] Enforce 3-minute cooldown between scaling?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("HEALTH & RELIABILITY")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	// config.HealthChecks = getUserConfirmation("  [REQ-5.1] Enable /health endpoint checks?")
	// if config.HealthChecks {
	// 	config.HealthCheckRecovery = getUserConfirmation("    [REQ-5.3] Terminate unhealthy instances?")
	// 	config.HealthCheckLogging = getUserConfirmation("    [REQ-5.4] Log health check failures?")
	// }

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
	// config.PrometheusMetrics = getUserConfirmation("  [REQ-6.1] Export metrics in Prometheus format?")
	// if config.PrometheusMetrics {
	// 	config.MetricsRetention = getUserConfirmation("    [REQ-6.3] Retain metrics for 7 days?")
	// }
	// config.ScalingLogging = getUserConfirmation("  [REQ-6.4] Log all scaling events?")

	fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("SECURITY (Advanced)")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	// config.TLSCommunication = getUserConfirmation("  [SEC-1] Use TLS for service-to-service communication?")
	// config.EncryptedState = getUserConfirmation("  [SEC-4] Encrypt Terraform state at rest?")

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
	fmt.Println("  METRICS:")
	fmt.Printf("    [%s] CPU Monitoring\n", map[bool]string{true: "✓", false: "✗"}[config.CPUMonitoring])
	fmt.Printf("    [%s] Request Rate Monitoring\n", map[bool]string{true: "✓", false: "✗"}[config.RequestRateMonitoring])
	// fmt.Println("\n  SCALING BEHAVIOR:")
	// fmt.Printf("    [%s] Immediate Scaling\n", map[bool]string{true: "✓", false: "✗"}[config.ImmediateScaling])
	// fmt.Printf("    [%s] 2-Min Scale-Up Window\n", map[bool]string{true: "✓", false: "✗"}[config.ScaleUpTimeWindow])
	// fmt.Printf("    [%s] 5-Min Scale-Down Window\n", map[bool]string{true: "✓", false: "✗"}[config.ScaleDownTimeWindow])
	// fmt.Println("\n  LIMITS & SAFETY:")
	// fmt.Printf("    [%s] Enforce Min 2 Instances\n", map[bool]string{true: "✓", false: "✗"}[config.EnforceMinInstances])
	// fmt.Printf("    [%s] Enforce Max 10 Instances\n", map[bool]string{true: "✓", false: "✗"}[config.EnforceMaxInstances])
	// fmt.Printf("    [%s] 3-Min Cooldown Period\n", map[bool]string{true: "✓", false: "✗"}[config.EnforceCooldown])
	// fmt.Println("\n  HEALTH & RELIABILITY:")
	// fmt.Printf("    [%s] Health Checks\n", map[bool]string{true: "✓", false: "✗"}[config.HealthChecks])
	// fmt.Printf("    [%s] Health Check Recovery\n", map[bool]string{true: "✓", false: "✗"}[config.HealthCheckRecovery])
	// fmt.Printf("    [%s] Health Check Logging\n", map[bool]string{true: "✓", false: "✗"}[config.HealthCheckLogging])
	fmt.Println("\n  SERVICE DISCOVERY & LB:")
	fmt.Printf("    [%s] Service Discovery\n", map[bool]string{true: "✓", false: "✗"}[config.ServiceDiscovery])
	fmt.Printf("    [%s] Load Balancing\n", map[bool]string{true: "✓", false: "✗"}[config.LoadBalancing])
	fmt.Printf("    [%s] Sticky Sessions\n", map[bool]string{true: "✓", false: "✗"}[config.StickySessionsLB])
	// fmt.Println("\n  OBSERVABILITY:")
	// fmt.Printf("    [%s] Prometheus Metrics\n", map[bool]string{true: "✓", false: "✗"}[config.PrometheusMetrics])
	// fmt.Printf("    [%s] Metrics Retention (7d)\n", map[bool]string{true: "✓", false: "✗"}[config.MetricsRetention])
	// fmt.Printf("    [%s] Scaling Event Logging\n", map[bool]string{true: "✓", false: "✗"}[config.ScalingLogging])
	// fmt.Println("\n  SECURITY:")
	// fmt.Printf("    [%s] TLS Communication\n", map[bool]string{true: "✓", false: "✗"}[config.TLSCommunication])
	// fmt.Printf("    [%s] Encrypted Terraform State\n", map[bool]string{true: "✓", false: "✗"}[config.EncryptedState])
	fmt.Println()
	
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

	var allIPs []string
	if err := json.Unmarshal(outBytes, &allIPs); err != nil {
		log.Fatalf("Failed to parse IPs from terraform output: %v", err)
	}

	appOutCmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
	appOutBytes, err := appOutCmd.Output()
	if err != nil {
		log.Fatalf("Failed to get app instance IPs from terraform output: %v", err)
	}

	var appIPs []string
	if err := json.Unmarshal(appOutBytes, &appIPs); err != nil {
		log.Fatalf("Failed to parse app IPs from terraform output: %v", err)
	}

	lbOutCmd := exec.Command("terraform", "output", "-raw", "lb_ip")
	lbIPRaw, err := lbOutCmd.Output()
	if err != nil {
		log.Fatalf("Failed to get LB IP from terraform output: %v", err)
	}
	lbIP := strings.TrimSpace(string(lbIPRaw))

	if len(allIPs) == 0 {
		log.Fatal("No instance IPs found in terraform output")
	}
	if len(appIPs) == 0 {
		log.Fatal("No app instance IPs found in terraform output")
	}

	fmt.Printf("Found IPs: %s\n", strings.Join(allIPs, ", "))
	fmt.Printf("Load balancer sender IP: %s\n", lbIP)
	fmt.Printf("Scalable app IPs: %s\n", strings.Join(appIPs, ", "))

	go startDashboard(&allIPs, &appIPs, &lbIP)
	time.AfterFunc(1*time.Second, func() {
		openBrowser("http://localhost:9090")
	})

	// Periodically refresh IPs from terraform state so dashboard always shows current instances
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			allOutCmd := exec.Command("terraform", "output", "-json", "instance_ips")
			allOutBytes, err := allOutCmd.Output()
			if err != nil {
				// Silently skip on error (e.g., terraform locked during apply)
				continue
			}
			var newAllIPs []string
			if err := json.Unmarshal(allOutBytes, &newAllIPs); err != nil {
				continue
			}

			appOutCmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
			appOutBytes, err := appOutCmd.Output()
			if err != nil {
				continue
			}
			var newAppIPs []string
			if err := json.Unmarshal(appOutBytes, &newAppIPs); err != nil {
				continue
			}

			lbOutCmd := exec.Command("terraform", "output", "-raw", "lb_ip")
			lbRaw, err := lbOutCmd.Output()
			if err != nil {
				continue
			}
			newLBIP := strings.TrimSpace(string(lbRaw))

			if len(newAllIPs) != len(allIPs) || !slicesEqual(allIPs, newAllIPs) || len(newAppIPs) != len(appIPs) || !slicesEqual(appIPs, newAppIPs) || lbIP != newLBIP {
				allIPs = newAllIPs
				appIPs = newAppIPs
				lbIP = newLBIP
				log.Printf("[IP REFRESH] Updated all instances: %v", allIPs)
				log.Printf("[IP REFRESH] Updated app instances: %v", appIPs)
			}
		}
	}()

	fmt.Println("=== Starting Scaler ===")
	args := append([]string{"run", "scaler.go"}, appIPs...)
	scalerCmd := exec.Command("go", args...)
	scalerCmd.Stdout = os.Stdout
	scalerCmd.Stderr = os.Stderr
	if err := scalerCmd.Start(); err != nil {
		log.Fatalf("Scaler failed to start: %v", err)
	}

	fmt.Println("=== Waiting 2 minutes for instances to boot and initialize ===")
	time.Sleep(2 * time.Minute)

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     STARTING TEST EXECUTION                                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	// Keeping the deploy flow CPU-only for dashboard validation.
	// if scalingConfig.EnforceMinInstances {
	// 	fmt.Println("[TEST] Verifying minimum 2 instances requirement...")
	// 	fmt.Printf("  Current instances: %d (Expected: >= 2)\n", len(ips))
	// 	if len(ips) >= 2 {
	// 		fmt.Println("  ✓ PASS: Minimum instance requirement met\n")
	// 	} else {
	// 		fmt.Println("  ✗ FAIL: Minimum instance requirement NOT met\n")
	// 	}
	// }

	// if scalingConfig.EnforceMaxInstances {
	// 	fmt.Println("[TEST] Verifying maximum 10 instances limit...")
	// 	fmt.Println("  This will be tested during stress test (max 10 instances allowed)\n")
	// }

	// if scalingConfig.EnforceCooldown {
	// 	fmt.Println("[TEST] Cooldown period is configured for 3 minutes between scaling operations")
	// 	fmt.Println("  Monitoring logs for cooldown enforcement...\n")
	// }

	// if scalingConfig.HealthChecks {
	// 	fmt.Println("[TEST] Testing /health endpoint on each instance...")
	// 	testHealthEndpoints(ips)
	// 	fmt.Println()
	// }

	// if scalingConfig.PrometheusMetrics {
	// 	fmt.Println("[TEST] Testing /metrics endpoint (Prometheus format)...")
	// 	testMetricsEndpoints(ips)
	// 	fmt.Println()
	// }

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     LAUNCHING REQUIREMENT-BASED TESTS                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝\n")

	stressedIPs := make(map[string]bool)
	var wg sync.WaitGroup

	if scalingConfig.CPUMonitoring {
		fmt.Println("[TEST GENERATOR] CPU Load Test - Will generate CPU load to trigger scale-up")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runCPULoadTest(scalingConfig, getActiveIPs)
		}()
	}

	if scalingConfig.RequestRateMonitoring {
		fmt.Println("[TEST GENERATOR] Traffic Spike Test - Will blast instances with traffic to trigger scale-up")
		wg.Add(1)
		go func() {
			defer wg.Done()
			runTrafficSpikeTest(scalingConfig, getLoadBalancerIP)
		}()
	}

	// if scalingConfig.HealthChecks && scalingConfig.HealthCheckRecovery {
	// 	fmt.Println("[TEST GENERATOR] Health Check Test - Will monitor instance health and recovery")
	// 	wg.Add(1)
	// 	go func() {
	// 		defer wg.Done()
	// 		runHealthCheckTest(scalingConfig, getActiveIPs)
	// 	}()
	// }

	// if scalingConfig.ScaleDownTimeWindow {
	// 	fmt.Println("[TEST GENERATOR] Scale-Down Test - Will reduce load after grace period to trigger scale-down")
	// 	wg.Add(1)
	// 	go func() {
	// 		defer wg.Done()
	// 		runScaleDownTest(scalingConfig, getActiveIPs)
	// 	}()
	// }

	// if scalingConfig.EnforceCooldown {
	// 	fmt.Println("[TEST GENERATOR] Cooldown Test - Will verify 3-minute cooldown between operations")
	// }

	// if scalingConfig.PrometheusMetrics {
	// 	fmt.Println("[TEST GENERATOR] Prometheus Metrics Test - Continuous metric validation")
	// 	wg.Add(1)
	// 	go func() {
	// 		defer wg.Done()
	// 		runMetricsValidationTest(scalingConfig, getActiveIPs)
	// 	}()
	// }

	fmt.Println()

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
				go stressTest(ip, 5, 20*time.Second)
			}
			time.Sleep(30 * time.Second)
		}
	}()

	scalerCmd.Wait()
}

func getActiveIPs() []string {
	cmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
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

func getLoadBalancerIP() string {
	cmd := exec.Command("terraform", "output", "-raw", "lb_ip")
	outBytes, err := cmd.Output()
	if err != nil {
		log.Printf("Failed to refresh LB IP: %v", err)
		return ""
	}
	return strings.TrimSpace(string(outBytes))
}

func stressTest(ip string, concurrency int, duration time.Duration) {
	url := fmt.Sprintf("http://%s:8080/work", ip)
	if strings.Contains(ip, ":") {
		url = fmt.Sprintf("http://%s/work", ip)
	}
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
			intensity = 25
		case 2:
			intensity = 75
		case 3:
			intensity = 5
		}

		phaseStart := time.Now()
		fmt.Printf("\n[CPU TEST PHASE %d] Intensity: %d workers/instance, Duration: 60s\n", phase, intensity)

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

func runTrafficSpikeTest(config ScalingConfig, getLBIP func() string) {
	fmt.Println("\n[TRAFFIC SPIKE TEST] Starting request rate load pattern...")
	fmt.Println("  Phase 1: Warm-up (30 seconds) - 100 req/s")
	fmt.Println("  Phase 2: Ramp-up (30 seconds) - 500 req/s")
	fmt.Println("  Phase 3: SPIKE (60 seconds) - 1500 req/s (exceeds 20 req/s threshold)")
	fmt.Println("  Expected: Scale-up after Phase 3 completes (waiting for 2-minute window)\n")

	time.Sleep(10 * time.Second)

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
		lbIP := getLBIP()
		if lbIP == "" {
			continue
		}

		fmt.Printf("\n[TRAFFIC PHASE %d] %s via LB %s - %d concurrent workers\n", phaseIdx+1, phase.name, lbIP, phase.maxWorkers)
		fmt.Printf("  Estimated requests/sec: ~%d (threshold is 20)\n", phase.maxWorkers*10)

		phaseStart := time.Now()
		var wg sync.WaitGroup
		wg.Add(1)
		go func(targetLB string) {
			defer wg.Done()
			stressTest(targetLB+":80", phase.maxWorkers, phase.duration)
		}(lbIP)

		wg.Wait()
		elapsed := time.Since(phaseStart)
		fmt.Printf("[TRAFFIC PHASE %d] Complete - Actual duration: %v\n", phaseIdx+1, elapsed)
	}

	fmt.Println("\n[TRAFFIC SPIKE TEST] Complete")
	fmt.Println("[TRAFFIC SPIKE TEST] Check ./metrics or scaler logs to verify request rate was captured\n")
}

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

func runScaleDownTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[SCALE-DOWN TEST] Starting scale-down validation...")
	fmt.Println("  Will maintain minimal load for 5+ minutes")
	fmt.Println("  Expected: System should scale-down after 5-minute low-load window\n")

	time.Sleep(4 * time.Minute)

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
