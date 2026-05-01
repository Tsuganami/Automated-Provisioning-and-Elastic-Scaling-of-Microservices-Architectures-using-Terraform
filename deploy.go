//go:build !scaler

package main

import (
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const instanceStartupGracePeriod = 4 * time.Minute

const (
	workBurnDuration    = 30 * time.Second
	spikeDuration       = 5 * time.Minute
	cpuLoadTestDuration = 3 * time.Minute
	healthFailDuration  = 2 * time.Minute
)

const (
	phaseAwaitingConfig int32 = 0
	phaseProvisioning   int32 = 1
	phaseReady          int32 = 2
)

var (
	systemPhase      atomic.Int32
	configReceivedCh = make(chan map[string]interface{}, 1)
	userConfigMu     sync.RWMutex
	userConfig       map[string]interface{}
)

const provisioningPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Provisioning - APS</title>
<script src="https://cdn.tailwindcss.com"></script>
<style>
  body{background:radial-gradient(circle at top left,rgba(59,130,246,.22),transparent 30%),radial-gradient(circle at top right,rgba(16,185,129,.16),transparent 24%),linear-gradient(180deg,#030712 0%,#0f172a 55%,#020617 100%);min-height:100vh;color:#fff;font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center}
  .spinner{width:48px;height:48px;border:4px solid rgba(255,255,255,.15);border-top-color:#38bdf8;border-radius:50%;animation:spin 1s linear infinite}
  @keyframes spin{to{transform:rotate(360deg)}}
</style>
</head>
<body>
  <div class="text-center max-w-lg px-6">
    <p class="text-xs uppercase tracking-[0.35em] text-sky-300/80">System Initialization</p>
    <h1 class="mt-2 text-3xl md:text-4xl font-semibold">Provisioning Infrastructure</h1>
    <p class="mt-3 text-sm md:text-base text-slate-300">Terraform is creating your instances. This typically takes 1-2 minutes. The dashboard will load automatically when ready.</p>
    <div class="mt-8 flex justify-center"><div class="spinner"></div></div>
    <p id="status" class="mt-6 text-xs text-slate-400">Starting...</p>
  </div>
<script>
  const status = document.getElementById('status');
  let attempts = 0;
  async function poll(){
    attempts++;
    try {
      const r = await fetch('/api/ready', { cache: 'no-store' });
      const j = await r.json();
      if (j.ready) {
        status.textContent = 'Ready - loading dashboard...';
        setTimeout(()=>{ window.location.href = '/'; }, 400);
        return;
      }
      status.textContent = 'Provisioning... (' + attempts + ')';
    } catch (e) {
      status.textContent = 'Waiting for backend... (' + attempts + ')';
    }
    setTimeout(poll, 2000);
  }
  poll();
</script>
</body>
</html>`

func setupLogging() {
	logFile, err := os.OpenFile("log.txt", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("WARNING: could not open log.txt for writing: %v\n", err)
		return
	}

	origStdout := os.Stdout
	origStderr := os.Stderr
	mw := io.MultiWriter(origStdout, logFile)
	mwErr := io.MultiWriter(origStderr, logFile)

	rOut, wOut, err := os.Pipe()
	if err == nil {
		os.Stdout = wOut
		go io.Copy(mw, rOut)
	}

	rErr, wErr, err := os.Pipe()
	if err == nil {
		os.Stderr = wErr
		go io.Copy(mwErr, rErr)
	}

	log.SetOutput(mw)
	fmt.Printf("=== Logging session output to log.txt (%s) ===\n", time.Now().Format(time.RFC3339))
}

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

func fetchMetrics(ip string) (float64, float64, float64, error) {
	client := http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, 0, 0, err
	}

	var cpuValue float64
	var requestCount float64
	var networkBytes float64
	var foundCPU bool
	var foundRequests bool
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "cpu_utilization ") || strings.HasPrefix(line, "request_count ") || strings.HasPrefix(line, "network_bytes_total ") {
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
			case "network_bytes_total":
				networkBytes = value
			}
		}
	}

	if !foundCPU && !foundRequests {
		return 0, 0, 0, fmt.Errorf("metrics not found")
	}

	return cpuValue, requestCount, networkBytes, nil
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
	IP                string  `json:"ip"`
	Role              string  `json:"role"`
	CPUPercent        float64 `json:"cpu_percent"`
	RequestCount      float64 `json:"request_count"`
	RequestRate       float64 `json:"request_rate"`
	NetworkThroughput float64 `json:"network_throughput"`
	Status            string  `json:"status"`
	Healthy           bool    `json:"healthy"`
	StartedAt         string  `json:"started_at,omitempty"`
	Error             string  `json:"error,omitempty"`
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
	lastNetworkByIP := make(map[string]float64)
	lastNetworkTimeByIP := make(map[string]time.Time)
	requestLoadRPS := 0

	removedIPs := make(map[string]bool)
	var removedMu sync.Mutex
	for _, ip := range *allIPs {
		instanceSeenAt[ip] = time.Now()
	}

	go func() {
		transport := &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			IdleConnTimeout:     30 * time.Second,
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		subtick := 0
		for range ticker.C {
			requestLoadMu.RLock()
			currentRPS := requestLoadRPS
			requestLoadMu.RUnlock()

			if currentRPS <= 0 {
				subtick = 0
				continue
			}

			targetLB := strings.TrimSpace(*lbIP)
			if targetLB == "" {
				continue
			}

			perTick := currentRPS / 20
			remainder := currentRPS % 20
			n := perTick
			if subtick < remainder {
				n++
			}
			subtick = (subtick + 1) % 20

			for i := 0; i < n; i++ {
				go func(targetIP string) {
					resp, err := client.Get(fmt.Sprintf("http://%s/request", targetIP))
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}(targetLB)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			requestLoadMu.RLock()
			currentRPS := requestLoadRPS
			requestLoadMu.RUnlock()
			targetLB := strings.TrimSpace(*lbIP)
			if targetLB == "" {
				targetLB = "(LB pending)"
			}
			log.Printf("[TRAFFIC STATUS] Sustained load: %d req/min -> %s", currentRPS*60, targetLB)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		switch systemPhase.Load() {
		case phaseAwaitingConfig:
			http.ServeFile(w, r, "templates/startup.html")
		case phaseProvisioning:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, provisioningPageHTML)
		default:
			http.ServeFile(w, r, "templates/index.html")
		}
	})

	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if systemPhase.Load() != phaseAwaitingConfig {
			http.Error(w, "configuration already submitted", http.StatusConflict)
			return
		}
		var cfg map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
			return
		}
		userConfigMu.Lock()
		userConfig = cfg
		userConfigMu.Unlock()

		if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = ioutil.WriteFile("user_config.json", data, 0644)
		}

		applyUserConfig(cfg)

		select {
		case configReceivedCh <- cfg:
		default:
		}
		log.Println("[STARTUP] User configuration received - beginning provisioning")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		phase := systemPhase.Load()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"phase": phase,
			"ready": phase == phaseReady,
		})
	})

	http.HandleFunc("/api/instance-removed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ip := strings.TrimSpace(r.URL.Query().Get("ip"))
		if ip == "" {
			http.Error(w, "missing ip query parameter", http.StatusBadRequest)
			return
		}
		removedMu.Lock()
		removedIPs[ip] = true
		removedMu.Unlock()
		log.Printf("[INSTANCE REMOVED] %s marked as destroying - hidden from dashboard", ip)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"ip":     ip,
		})
	})

	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		statuses := make([]instanceStatus, 0, len(*allIPs))
		for _, ip := range *allIPs {
			removedMu.Lock()
			gone := removedIPs[ip]
			removedMu.Unlock()
			if gone {
				continue
			}
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
				IP:        ip,
				Role:      role,
				Healthy:   true,
				Status:    "ACTIVE",
				StartedAt: seenAt.UTC().Format(time.RFC3339),
			}
			if role == "LB-SENDER" {
				if err := checkLBHealth(ip); err != nil {
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

			cpu, requests, netBytes, err := fetchMetrics(ip)
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

			prevNet, hasPrevNet := lastNetworkByIP[ip]
			prevNetAt, hasPrevNetAt := lastNetworkTimeByIP[ip]
			if hasPrevNet && hasPrevNetAt {
				elapsed := time.Since(prevNetAt).Seconds()
				if elapsed > 0 {
					delta := netBytes - prevNet
					if delta < 0 {
						delta = 0
					}
					status.NetworkThroughput = delta / elapsed
				}
			}
			lastNetworkByIP[ip] = netBytes
			lastNetworkTimeByIP[ip] = time.Now()
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
		visibleCount := 0
		for _, ip := range *allIPs {
			removedMu.Lock()
			gone := removedIPs[ip]
			removedMu.Unlock()
			if gone {
				continue
			}
			visibleCount++
			if ip == *lbIP {
				err := checkLBHealth(ip)
				if err != nil {
					unhealthyCount++
				} else {
					healthyCount++
				}
				continue
			}

			cpu, requests, _, err := fetchMetrics(ip)
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
			"instance_count":  visibleCount,
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

		targetLB := strings.TrimSpace(*lbIP)
		if targetLB == "" {
			http.Error(w, "load balancer ip is unavailable", http.StatusServiceUnavailable)
			return
		}

		burst := 1
		go func(targetIP string) {
			client := &http.Client{Timeout: 35 * time.Second}
			for i := 0; i < burst; i++ {
				go func() {
					resp, err := client.Get(fmt.Sprintf("http://%s/work", targetIP))
					if err != nil {
						log.Printf("[DASHBOARD LOAD] Failed to hit /work via LB %s: %v", targetIP, err)
						return
					}
					resp.Body.Close()
				}()
			}
		}(targetLB)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "accepted",
			"lb_ip":    targetLB,
			"requests": burst,
		})
	})

	http.HandleFunc("/api/fail-health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		targetIP := strings.TrimSpace(r.URL.Query().Get("ip"))
		if targetIP == "" {
			http.Error(w, "missing ip query parameter", http.StatusBadRequest)
			return
		}

		isApp := false
		for _, ip := range *appIPs {
			if ip == targetIP {
				isApp = true
				break
			}
		}
		if !isApp {
			http.Error(w, "ip is not a known app instance", http.StatusBadRequest)
			return
		}

		seconds := 120
		if s := strings.TrimSpace(r.URL.Query().Get("seconds")); s != "" {
			if parsed, err := strconv.Atoi(s); err == nil && parsed > 0 {
				seconds = parsed
			}
		}

		client := &http.Client{Timeout: 5 * time.Second}
		scalerURL := fmt.Sprintf("http://127.0.0.1:9091/replace?ip=%s", targetIP)
		resp, err := client.Post(scalerURL, "application/json", nil)
		if err != nil {
			log.Printf("[FAIL-HEALTH] Could not reach scaler control endpoint: %v", err)
			http.Error(w, fmt.Sprintf("scaler unreachable: %v", err), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			body, _ := ioutil.ReadAll(resp.Body)
			http.Error(w, fmt.Sprintf("scaler rejected request: %s", strings.TrimSpace(string(body))), resp.StatusCode)
			return
		}

		log.Printf("[FAIL-HEALTH] %s marked for replacement (traffic rerouted)", targetIP)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "replacing",
			"ip":      targetIP,
			"seconds": seconds,
		})
	})

	http.HandleFunc("/api/request-load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		targetLB := strings.TrimSpace(*lbIP)
		if targetLB == "" {
			http.Error(w, "load balancer ip is unavailable", http.StatusServiceUnavailable)
			return
		}

		const increaseByRPS = 5
		requestLoadMu.Lock()
		requestLoadRPS += increaseByRPS
		currentRPS := requestLoadRPS
		requestLoadMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "accepted",
			"lb_ip":       targetLB,
			"added_rps":   increaseByRPS,
			"current_rps": currentRPS,
		})
	})

	http.HandleFunc("/api/spike-node", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		targetIP := strings.TrimSpace(r.URL.Query().Get("ip"))
		if targetIP == "" {
			http.Error(w, "missing ip query parameter", http.StatusBadRequest)
			return
		}

		isApp := false
		for _, ip := range *appIPs {
			if ip == targetIP {
				isApp = true
				break
			}
		}
		if !isApp {
			http.Error(w, "ip is not a known app instance", http.StatusBadRequest)
			return
		}

		go func(ip string) {
			client := &http.Client{Timeout: 8 * time.Second}
			resp, err := client.Get(fmt.Sprintf("http://%s:8080/spike", ip))
			if err != nil {
				log.Printf("[SPIKE] Failed to trigger spike on %s: %v", ip, err)
				return
			}
			resp.Body.Close()
			log.Printf("[SPIKE] CPU spike injected on %s - self-healer should replace it shortly", ip)
		}(targetIP)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "accepted",
			"ip":     targetIP,
		})
	})

	http.HandleFunc("/api/test/traffic-spike", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		targetLB := strings.TrimSpace(*lbIP)
		if targetLB == "" {
			http.Error(w, "load balancer ip is unavailable", http.StatusServiceUnavailable)
			return
		}

		const stepRPS = 10
		requestLoadMu.Lock()
		requestLoadRPS += stepRPS
		currentRPS := requestLoadRPS
		requestLoadMu.Unlock()

		currentRPM := currentRPS * 60
		log.Printf("[TRAFFIC SPIKE] +600 req/min -> sustained load now %d req/min via LB %s", currentRPM, targetLB)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "accepted",
			"lb_ip":       targetLB,
			"added_rpm":   600,
			"current_rpm": currentRPM,
			"message":     fmt.Sprintf("Sustained traffic increased by 600 req/min. Current load: %d req/min through LB %s.", currentRPM, targetLB),
		})
	})

	http.HandleFunc("/api/test/traffic-reduce", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		targetLB := strings.TrimSpace(*lbIP)

		const stepRPS = 10
		requestLoadMu.Lock()
		requestLoadRPS -= stepRPS
		if requestLoadRPS < 0 {
			requestLoadRPS = 0
		}
		currentRPS := requestLoadRPS
		requestLoadMu.Unlock()

		currentRPM := currentRPS * 60
		log.Printf("[TRAFFIC REDUCE] -600 req/min -> sustained load now %d req/min", currentRPM)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":      "accepted",
			"lb_ip":       targetLB,
			"removed_rpm": 600,
			"current_rpm": currentRPM,
			"message":     fmt.Sprintf("Sustained traffic reduced by 600 req/min. Current load: %d req/min.", currentRPM),
		})
	})

	http.HandleFunc("/api/test/cpu-load", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		targetLB := strings.TrimSpace(*lbIP)
		if targetLB == "" {
			http.Error(w, "load balancer ip is unavailable", http.StatusServiceUnavailable)
			return
		}

		go func(lb string) {
			log.Printf("[CPU LOAD TEST] Sending /work bursts via LB %s", lb)
			client := &http.Client{Timeout: 35 * time.Second}
			deadline := time.Now().Add(cpuLoadTestDuration)
			for time.Now().Before(deadline) {
				for i := 0; i < 20; i++ {
					go func() {
						resp, err := client.Get(fmt.Sprintf("http://%s/work", lb))
						if err == nil {
							resp.Body.Close()
						}
					}()
				}
				time.Sleep(1 * time.Second)
			}
			log.Printf("[CPU LOAD TEST] Complete")
		}(targetLB)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "accepted",
			"lb_ip":   targetLB,
			"message": fmt.Sprintf("CPU load test started via load balancer %s (%s of /work bursts).", targetLB, cpuLoadTestDuration),
		})
	})

	http.HandleFunc("/api/test/durations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"work_burn_seconds":   int(workBurnDuration.Seconds()),
			"spike_seconds":       int(spikeDuration.Seconds()),
			"cpu_load_seconds":    int(cpuLoadTestDuration.Seconds()),
			"health_fail_seconds": int(healthFailDuration.Seconds()),
		})
	})

	fmt.Println("=== Dashboard running at http://localhost:9090 ===")
	if err := http.ListenAndServe(":9090", nil); err != nil {
		log.Fatalf("Dashboard server failed: %v", err)
	}
}

type ScalingConfig struct {
	CPUMonitoring         bool
	RequestRateMonitoring bool

	ImmediateScaling    bool
	ScaleUpTimeWindow   bool
	ScaleDownTimeWindow bool

	EnforceMinInstances bool
	EnforceMaxInstances bool

	EnforceCooldown bool

	HealthChecks        bool
	HealthCheckRecovery bool

	ServiceDiscovery bool
	LoadBalancing    bool
	StickySessionsLB bool

	PrometheusMetrics  bool
	MetricsRetention   bool
	HealthCheckLogging bool
	ScalingLogging     bool

	TLSCommunication bool
	EncryptedState   bool
}

func selectScalingOptions(yes bool) ScalingConfig {
	config := ScalingConfig{}
	if yes {
		config.CPUMonitoring = true
		config.RequestRateMonitoring = true
		config.ImmediateScaling = true
		config.ServiceDiscovery = true
		config.LoadBalancing = true
		config.StickySessionsLB = true
	} else {
		fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
		fmt.Println("║     CONFIGURE SCALING OPTIONS FOR THIS TEST                        ║")
		fmt.Println("╚════════════════════════════════════════════════════════════════════╝")

		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("SCALING METRICS (What triggers scaling)")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		config.CPUMonitoring = getUserConfirmation("  [REQ-1.1] Monitor CPU utilization?")
		config.RequestRateMonitoring = getUserConfirmation("  [REQ-1.2] Monitor incoming request rate?")

		fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("SCALING BEHAVIOR (How scaling happens)")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		config.ImmediateScaling = true

		fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("INSTANCE LIMITS & SAFETY")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

		fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("HEALTH & RELIABILITY")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

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

		fmt.Println("\n━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("SECURITY (Advanced)")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	}

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
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝")
	fmt.Println("  METRICS:")
	fmt.Printf("    [%s] CPU Monitoring\n", map[bool]string{true: "✓", false: "✗"}[config.CPUMonitoring])
	fmt.Printf("    [%s] Request Rate Monitoring\n", map[bool]string{true: "✓", false: "✗"}[config.RequestRateMonitoring])

	fmt.Println("\n  SERVICE DISCOVERY & LB:")
	fmt.Printf("    [%s] Service Discovery\n", map[bool]string{true: "✓", false: "✗"}[config.ServiceDiscovery])
	fmt.Printf("    [%s] Load Balancing\n", map[bool]string{true: "✓", false: "✗"}[config.LoadBalancing])
	fmt.Printf("    [%s] Sticky Sessions\n", map[bool]string{true: "✓", false: "✗"}[config.StickySessionsLB])

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

func initTerraform() {
	fmt.Println("=== Initializing Terraform ===")
	initCmd := exec.Command("terraform", "init")
	initCmd.Stdout = os.Stdout
	initCmd.Stderr = os.Stderr
	if err := initCmd.Run(); err != nil {
		log.Fatalf("Terraform init failed: %v", err)
	}
	fmt.Println("=== Terraform Initialization Complete ===")
}

func main() {
	setupLogging()

	skipStartupPage := false
	if len(os.Args) > 1 && (os.Args[1] == "-y" || os.Args[1] == "--yes") {
		skipStartupPage = true
	}

	scalingConfig := selectScalingOptions(true)

	var allIPs []string
	var appIPs []string
	var lbIP string

	go startDashboard(&allIPs, &appIPs, &lbIP)
	time.AfterFunc(1*time.Second, func() {
		openBrowser("http://localhost:9090")
	})

	var initialInstances = 1
	if skipStartupPage {
		log.Println("[STARTUP] -y flag set: skipping web configuration page")
	} else {
		fmt.Println("=== Awaiting startup configuration at http://localhost:9090 ===")
		cfg := <-configReceivedCh

		if v, ok := cfg["initial-instances"]; ok {
			switch n := v.(type) {
			case float64:
				initialInstances = int(n)
			case int:
				initialInstances = n
			case string:
				if parsed, err := strconv.Atoi(n); err == nil {
					initialInstances = parsed
				}
			}
		}
		if initialInstances < 1 {
			initialInstances = 1
		}
		log.Printf("[STARTUP] Initial instance count from config: %d", initialInstances)
	}

	systemPhase.Store(phaseProvisioning)

	initTerraform()

	fmt.Println("=== Running Terraform Apply ===")
	applyCmd := exec.Command("terraform", "apply",
		"-var", fmt.Sprintf("app_instance_count=%d", initialInstances),
		"-auto-approve", "-lock=false")
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

	if err := json.Unmarshal(outBytes, &allIPs); err != nil {
		log.Fatalf("Failed to parse IPs from terraform output: %v", err)
	}

	appOutCmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
	appOutBytes, err := appOutCmd.Output()
	if err != nil {
		log.Fatalf("Failed to get app instance IPs from terraform output: %v", err)
	}

	if err := json.Unmarshal(appOutBytes, &appIPs); err != nil {
		log.Fatalf("Failed to parse app IPs from terraform output: %v", err)
	}

	lbOutCmd := exec.Command("terraform", "output", "-raw", "lb_ip")
	lbIPRaw, err := lbOutCmd.Output()
	if err != nil {
		log.Fatalf("Failed to get LB IP from terraform output: %v", err)
	}
	lbIP = strings.TrimSpace(string(lbIPRaw))

	if len(allIPs) == 0 {
		log.Fatal("No instance IPs found in terraform output")
	}
	if len(appIPs) == 0 {
		log.Fatal("No app instance IPs found in terraform output")
	}

	fmt.Printf("Found IPs: %s\n", strings.Join(allIPs, ", "))
	fmt.Printf("Load balancer sender IP: %s\n", lbIP)
	fmt.Printf("Scalable app IPs: %s\n", strings.Join(appIPs, ", "))

	systemPhase.Store(phaseReady)

	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			allOutCmd := exec.Command("terraform", "output", "-json", "instance_ips")
			allOutBytes, err := allOutCmd.Output()
			if err != nil {

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

	scalerBin := "scaler.exe"
	if runtime.GOOS != "windows" {
		scalerBin = "./scaler"
	}
	buildCmd := exec.Command("go", "build", "-tags=scaler", "-o", scalerBin, ".")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		log.Fatalf("Failed to build scaler: %v", err)
	}
	scalerCmd := exec.Command(scalerBin, appIPs...)
	scalerCmd.Stdout = os.Stdout
	scalerCmd.Stderr = os.Stderr
	if err := scalerCmd.Start(); err != nil {
		log.Fatalf("Scaler failed to start: %v", err)
	}

	fmt.Println("=== Waiting 2 minutes for instances to boot and initialize ===")
	time.Sleep(2 * time.Minute)

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     STARTING TEST EXECUTION                                        ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝")

	fmt.Println("\n╔════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║     LAUNCHING REQUIREMENT-BASED TESTS                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════════╝")

	stressedIPs := make(map[string]bool)
	_ = sync.WaitGroup{}

	if scalingConfig.CPUMonitoring {
		fmt.Println("[INFO] CPU monitoring enabled - scaler will react to dashboard-triggered load.")
	}

	if scalingConfig.RequestRateMonitoring {
		fmt.Println("[INFO] Request rate monitoring enabled - scaler will react to dashboard-triggered traffic.")
	}

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
				fmt.Println("OK")
				stressedIPs[ip] = true
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
	fmt.Println("  Expected: Scale-up after Phase 2 completes (waiting for 2-minute window)")

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
	fmt.Println("[CPU LOAD TEST] Check scaler logs to verify scale-up was triggered")
}

func runTrafficSpikeTest(config ScalingConfig, getLBIP func() string) {
	fmt.Println("\n[TRAFFIC SPIKE TEST] Starting request rate load pattern...")
	fmt.Println("  Phase 1: Warm-up (30 seconds) - 100 req/s")
	fmt.Println("  Phase 2: Ramp-up (30 seconds) - 500 req/s")
	fmt.Println("  Phase 3: SPIKE (60 seconds) - 1500 req/s (exceeds 20 req/s threshold)")
	fmt.Println("  Expected: Scale-up after Phase 3 completes (waiting for 2-minute window)")

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
	fmt.Println("[TRAFFIC SPIKE TEST] Check ./metrics or scaler logs to verify request rate was captured")
}

func runHealthCheckTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[HEALTH CHECK TEST] Starting health check validation...")
	fmt.Println("  Will continuously verify /health endpoints")
	fmt.Println("  Monitors: Response time, status codes, failure tracking")

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
	fmt.Println("  Expected: System should scale-down after 5-minute low-load window")

	time.Sleep(4 * time.Minute)

	fmt.Println("[SCALE-DOWN TEST] Beginning 5-minute low-load period...")
	fmt.Println("  Sending 1 request every 10 seconds (minimal load)")
	fmt.Println("  CPU load: <10%")
	fmt.Println("  Request rate: ~0.1 req/s (far below 100 req/s threshold)")

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
	fmt.Println("[SCALE-DOWN TEST] Check scaler logs - should show scale-down triggered after 5-minute window")
}

func runMetricsValidationTest(config ScalingConfig, getActiveIPs func() []string) {
	fmt.Println("\n[METRICS VALIDATION TEST] Starting continuous metric validation...")
	fmt.Println("  Verifies Prometheus format every 30 seconds")

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
