package main

import (
    "errors"
    "fmt"
    "io/ioutil"
    "log"
    "net/http"
    "os"         
    "os/exec"
    "strconv"
    "strings"
    "time"
)

const (
	ThresholdUp          = 70.0
	ThresholdDown        = 30.0
	MinInstances         = 2
	MaxInstances         = 10  // REQ-1.6: Maximum 10 instances
	Cooldown             = 3 * time.Minute
	MonitoringInterval   = 30 * time.Second  // REQ-1.1: Monitor at 30-second intervals
	ScaleUpTimeWindow    = 2 * time.Minute   // REQ-1.3: Require 2 minutes above threshold
	ScaleDownTimeWindow  = 5 * time.Minute   // REQ-1.4: Require 5 minutes below threshold
	RequestRateThresholdUp   = 1000.0  // Requests per second to trigger scale-up
	RequestRateThresholdDown = 100.0   // Requests per second to trigger scale-down
	HealthCheckInterval  = 10 * time.Second // REQ-3.3: Health checks every 10 seconds
	HealthCheckRetries   = 2                 // REQ-3.4 & REQ-5.3: Remove after 2 consecutive failures
)

var currentInstances = 2
var lastScalingTime = time.Now().Add(-Cooldown)
var previousRequestCounts = make(map[string]float64) // Track request counts to calculate rate
var thresholdExceededTime time.Time                  // When we first exceeded threshold
var thresholdDroppedTime time.Time                   // When we first dropped below threshold
var healthCheckFailures = make(map[string]int)       // Track consecutive health check failures per instance


func getAverageCPU(ips []string) (float64, error) {
	var totalCPU float64
	var successCount int
	for _, ip := range ips {
		resp, err := http.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
		if err != nil {
			log.Printf("Error connecting to %s: %v", ip, err)
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("Error reading response from %s: %v", ip, err)
			continue
		}
		
		// Parse Prometheus format metrics - look for "cpu_utilization <value>"
		var cpuValue float64
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "cpu_utilization") && !strings.HasPrefix(line, "# ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					val, err := strconv.ParseFloat(parts[1], 64)
					if err == nil {
						cpuValue = val
						break
					}
				}
			}
		}
		
		totalCPU += cpuValue
		successCount++
	}
	if successCount == 0 {
		return 0, errors.New("could not retrieve data from any IP")
	}
	return totalCPU / float64(successCount), nil
}

// REQ-1.2: Monitor incoming request rate at 30-second intervals
func getAverageRequestRate(ips []string) (float64, error) {
	var totalRate float64
	var successCount int
	
	for _, ip := range ips {
		resp, err := http.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
		if err != nil {
			log.Printf("Error connecting to %s for request metrics: %v", ip, err)
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("Error reading request metrics from %s: %v", ip, err)
			continue
		}
		
		// Parse Prometheus format metrics - look for "request_count <value>"
		var requestCount float64
		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "request_count") && !strings.HasPrefix(line, "# ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					val, err := strconv.ParseFloat(parts[1], 64)
					if err == nil {
						requestCount = val
						break
					}
				}
			}
		}
		
		// Calculate request rate (difference from previous count / 30 seconds)
		previous, exists := previousRequestCounts[ip]
		if exists {
			ratePerSecond := (requestCount - previous) / 30.0 // 30 seconds interval
			totalRate += ratePerSecond
			log.Printf("[%s] Total Requests: %.0f, Rate: %.2f req/s", ip, requestCount, ratePerSecond)
		} else {
			log.Printf("[%s] Total Requests: %.0f (first sample - rate will calc next period)", ip, requestCount)
		}
		
		// Update previous count
		previousRequestCounts[ip] = requestCount
		successCount++
	}
	
	if successCount == 0 {
		return 0, errors.New("could not retrieve request rate from any IP")
	}
	
	avgRate := totalRate / float64(successCount)
	return avgRate, nil
}

// REQ-5.1 & REQ-5.2: Health check endpoint monitoring
func checkHealth(ips []string) map[string]bool {
	healthStatus := make(map[string]bool)
	client := http.Client{Timeout: 5 * time.Second}
	
	for _, ip := range ips {
		url := fmt.Sprintf("http://%s:8080/health", ip)
		resp, err := client.Get(url)
		
		if err != nil {
			healthStatus[ip] = false
			healthCheckFailures[ip]++
			log.Printf("[HEALTH CHECK FAIL] %s: %v (failure count: %d)", ip, err, healthCheckFailures[ip])
			continue
		}
		defer resp.Body.Close()
		
		// REQ-5.2: Health checks return HTTP 200 for healthy status
		if resp.StatusCode == http.StatusOK {
			healthStatus[ip] = true
			healthCheckFailures[ip] = 0  // Reset on success
			log.Printf("[HEALTH CHECK OK] %s: Healthy", ip)
		} else {
			healthStatus[ip] = false
			healthCheckFailures[ip]++
			log.Printf("[HEALTH CHECK FAIL] %s: HTTP %d (failure count: %d)", ip, resp.StatusCode, healthCheckFailures[ip])
		}
	}
	
	return healthStatus
}


func runTerraform(count int) {
	if time.Since(lastScalingTime) < Cooldown {
		fmt.Println("Cooldown period active, skipping...")
		return
	}

	fmt.Printf("--- STARTING TERRAFORM APPLY (Target: %d) ---\n", count)
	
	cmd := exec.Command("terraform", "apply", 
		"-var", fmt.Sprintf("instance_count=%d", count), 
		"-auto-approve")
	

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		fmt.Printf("Terraform failed: %v\n", err)
	} else {
		currentInstances = count
		lastScalingTime = time.Now()
		fmt.Printf("--- SUCCESS: Scaled to %d instances ---\n", count)
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run scaler.go <ip1> <ip2> ...")
	}
	ips := os.Args[1:]
	fmt.Printf("Monitoring IPs: %v\n", ips)

	// Start health check routine (runs every 10 seconds)
	go func() {
		ticker := time.NewTicker(HealthCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			healthStatus := checkHealth(ips)
			for ip, healthy := range healthStatus {
				if !healthy && healthCheckFailures[ip] > HealthCheckRetries {
					log.Printf("[ALERT] Instance %s unhealthy for %d checks - should be replaced", ip, healthCheckFailures[ip])
				}
			}
		}
	}()

	for {
		// REQ-1.1: Monitor CPU at 30-second intervals
		avgCPU, err := getAverageCPU(ips)
		if err != nil {
			log.Printf("Error retrieving CPU metrics: %v", err)
		}
		
		// REQ-1.2: Monitor request rate at 30-second intervals
		avgRequestRate, err := getAverageRequestRate(ips)
		if err != nil {
			log.Printf("Error retrieving request metrics: %v", err)
		}

		fmt.Printf("=== METRICS [%s] ===\n", time.Now().Format("15:04:05"))
		fmt.Printf("  Average CPU: %.2f%%\n", avgCPU)
		fmt.Printf("  Average Request Rate: %.2f req/s\n", avgRequestRate)
		fmt.Printf("  Current Instances: %d\n", currentInstances)
		fmt.Printf("  Thresholds: CPU [%.1f%% - %.1f%%], ReqRate [%.0f - %.0f req/s]\n", 
			ThresholdDown, ThresholdUp, RequestRateThresholdDown, RequestRateThresholdUp)

		// REQ-1.3: Scale-up decision with 2-minute time window
		scaleUpTriggered := (avgCPU > ThresholdUp || avgRequestRate > RequestRateThresholdUp)
		if scaleUpTriggered {
			if thresholdExceededTime.IsZero() {
				thresholdExceededTime = time.Now()
				log.Printf("[SCALE-UP MONITOR] Threshold exceeded, will wait 2 minutes before scaling")
			}
			
			timeAboveThreshold := time.Since(thresholdExceededTime)
			fmt.Printf("  Time above threshold: %v / 2m (scale-up trigger: %v)\n", timeAboveThreshold, timeAboveThreshold >= ScaleUpTimeWindow)
			
			if timeAboveThreshold >= ScaleUpTimeWindow && currentInstances < MaxInstances {
				reason := ""
				if avgCPU > ThresholdUp {
					reason += fmt.Sprintf("CPU: %.2f%% ", avgCPU)
				}
				if avgRequestRate > RequestRateThresholdUp {
					reason += fmt.Sprintf("RequestRate: %.2f req/s ", avgRequestRate)
				}
				fmt.Printf(">>> SCALE-UP TRIGGERED after 2 minutes (%s)\n\n", reason)
				runTerraform(currentInstances + 1)
				thresholdExceededTime = time.Time{} // Reset
			}
		} else {
			thresholdExceededTime = time.Time{} // Reset if we drop below threshold
		}

		// REQ-1.4: Scale-down decision with 5-minute time window
		scaleDownTriggered := (avgCPU < ThresholdDown && avgRequestRate < RequestRateThresholdDown)
		if scaleDownTriggered {
			if thresholdDroppedTime.IsZero() {
				thresholdDroppedTime = time.Now()
				log.Printf("[SCALE-DOWN MONITOR] Metrics dropped below threshold, will wait 5 minutes before scaling")
			}
			
			timeBelowThreshold := time.Since(thresholdDroppedTime)
			fmt.Printf("  Time below threshold: %v / 5m (scale-down trigger: %v)\n", timeBelowThreshold, timeBelowThreshold >= ScaleDownTimeWindow)
			
			if timeBelowThreshold >= ScaleDownTimeWindow && currentInstances > MinInstances {
				fmt.Printf(">>> SCALE-DOWN TRIGGERED after 5 minutes (CPU: %.2f%%, ReqRate: %.2f req/s)\n\n", avgCPU, avgRequestRate)
				runTerraform(currentInstances - 1)
				thresholdDroppedTime = time.Time{} // Reset
			}
		} else {
			thresholdDroppedTime = time.Time{} // Reset if we go back above threshold
		}

		time.Sleep(MonitoringInterval)  // REQ-1.1: 30-second monitoring interval
	}
}