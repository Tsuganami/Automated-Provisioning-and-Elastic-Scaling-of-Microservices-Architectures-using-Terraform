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
	ThresholdUp            = 50.0
	ThresholdDown          = 30.0
	MinInstances           = 1
	MaxInstances           = 3
	Cooldown               = 1 * time.Minute
	MonitoringInterval     = 30 * time.Second
	ScaleUpTimeWindow      = 30 * time.Second
	ScaleDownTimeWindow    = 5 * time.Minute
	RequestRateThresholdUp = 20.0
	RequestRateThresholdDown = 5.0
	HealthCheckInterval    = 10 * time.Second
	HealthCheckRetries     = 2
)

var currentInstances = 1
var lastScalingTime = time.Now().Add(-Cooldown)
var previousRequestCounts = make(map[string]float64)
var thresholdExceededTime time.Time
var thresholdDroppedTime time.Time
var healthCheckFailures = make(map[string]int)

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

		previous, exists := previousRequestCounts[ip]
		if exists {
			ratePerSecond := (requestCount - previous) / 30.0
			totalRate += ratePerSecond
			log.Printf("[%s] Total Requests: %.0f, Rate: %.2f req/s", ip, requestCount, ratePerSecond)
		} else {
			log.Printf("[%s] Total Requests: %.0f (first sample - rate will calc next period)", ip, requestCount)
		}

		previousRequestCounts[ip] = requestCount
		successCount++
	}

	if successCount == 0 {
		return 0, errors.New("could not retrieve request rate from any IP")
	}

	avgRate := totalRate / float64(successCount)
	return avgRate, nil
}

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

		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			healthStatus[ip] = true
			healthCheckFailures[ip] = 0
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

	cmd := exec.Command("terraform", "apply", "-var", fmt.Sprintf("app_instance_count=%d", count), "-auto-approve")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		fmt.Printf("Terraform failed: %v\n", err)
		return
	}

	currentInstances = count
	lastScalingTime = time.Now()
	fmt.Printf("--- SUCCESS: Scaled to %d instances ---\n", count)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run scaler.go <ip1> <ip2> ...")
	}

	ips := os.Args[1:]
	currentInstances = len(ips)
	fmt.Printf("Monitoring IPs: %v\n", ips)

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
		avgCPU, err := getAverageCPU(ips)
		if err != nil {
			log.Printf("Error retrieving CPU metrics: %v", err)
		}

		avgRequestRate, err := getAverageRequestRate(ips)
		if err != nil {
			log.Printf("Error retrieving request metrics: %v", err)
		}

		fmt.Printf("=== METRICS [%s] ===\n", time.Now().Format("15:04:05"))
		fmt.Printf("  Average CPU: %.2f%%\n", avgCPU)
		fmt.Printf("  Average Request Rate: %.2f req/s\n", avgRequestRate)
		fmt.Printf("  Current Instances: %d\n", currentInstances)
		fmt.Printf("  Thresholds: CPU [%.1f%% - %.1f%%], ReqRate [%.0f - %.0f req/s]\n", ThresholdDown, ThresholdUp, RequestRateThresholdDown, RequestRateThresholdUp)

		scaleUpTriggered := avgCPU > ThresholdUp || avgRequestRate > RequestRateThresholdUp
		if scaleUpTriggered {
			if thresholdExceededTime.IsZero() {
				thresholdExceededTime = time.Now()
				log.Printf("[SCALE-UP MONITOR] Threshold exceeded, will wait 30 seconds before scaling")
			}

			timeAboveThreshold := time.Since(thresholdExceededTime)
			fmt.Printf("  Time above threshold: %v / 30s (scale-up trigger: %v)\n", timeAboveThreshold, timeAboveThreshold >= ScaleUpTimeWindow)

			if timeAboveThreshold >= ScaleUpTimeWindow && currentInstances < MaxInstances {
				reason := ""
				if avgCPU > ThresholdUp {
					reason += fmt.Sprintf("CPU: %.2f%% ", avgCPU)
				}
				if avgRequestRate > RequestRateThresholdUp {
					reason += fmt.Sprintf("RequestRate: %.2f req/s ", avgRequestRate)
				}
				fmt.Printf(">>> SCALE-UP TRIGGERED after 30 seconds (%s)\n\n", reason)
				runTerraform(currentInstances + 1)
				thresholdExceededTime = time.Time{}
			}
		} else {
			thresholdExceededTime = time.Time{}
		}

		scaleDownTriggered := avgCPU < ThresholdDown && avgRequestRate < RequestRateThresholdDown
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
				thresholdDroppedTime = time.Time{}
			}
		} else {
			thresholdDroppedTime = time.Time{}
		}

		time.Sleep(MonitoringInterval)
	}
}