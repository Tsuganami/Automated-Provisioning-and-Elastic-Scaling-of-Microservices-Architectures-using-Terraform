package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ThresholdUp               = 50.0
	ThresholdDown             = 30.0
	MinInstances              = 1
	MaxInstances              = 3
	Cooldown                  = 1 * time.Minute
	MonitoringInterval        = 30 * time.Second
	ScaleUpTimeWindow         = 30 * time.Second
	ScaleDownTimeWindow       = 5 * time.Minute
	RequestRateThresholdUp    = 20.0
	RequestRateThresholdDown  = 5.0
	HealthCheckInterval       = 10 * time.Second
	HealthCheckRetries        = 2
	InstanceStartupGracePeriod = 4 * time.Minute
)

var currentInstances = 1
var lastScalingTime = time.Now().Add(-Cooldown)
var previousRequestCounts = make(map[string]float64)
var thresholdExceededTime time.Time
var thresholdDroppedTime time.Time
var healthCheckFailures = make(map[string]int)
var replacementInProgress = make(map[string]bool)
var instanceStartupTime = make(map[string]time.Time) // Track when each instance started
var autoReplaceUnhealthy bool
var terraformOperationMu sync.Mutex
var stateMu sync.Mutex
var monitoredIPsMu sync.RWMutex
var monitoredIPs []string

func parseBoolEnv(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes" || value == "y" || value == "on"
}

func setMonitoredIPs(ips []string) {
	copied := append([]string(nil), ips...)
	monitoredIPsMu.Lock()
	monitoredIPs = copied
	monitoredIPsMu.Unlock()
	currentInstances = len(copied)

	allowed := make(map[string]struct{}, len(copied))
	for _, ip := range copied {
		allowed[ip] = struct{}{}
	}

	stateMu.Lock()
	for ip := range previousRequestCounts {
		if _, exists := allowed[ip]; !exists {
			delete(previousRequestCounts, ip)
		}
	}
	for ip := range healthCheckFailures {
		if _, exists := allowed[ip]; !exists {
			delete(healthCheckFailures, ip)
		}
	}
	for ip := range replacementInProgress {
		if _, exists := allowed[ip]; !exists {
			delete(replacementInProgress, ip)
		}
	}
	// Track startup time for new instances
	for _, ip := range copied {
		if _, exists := instanceStartupTime[ip]; !exists {
			instanceStartupTime[ip] = time.Now()
		}
	}
	// Clean up startup times for removed instances
	for ip := range instanceStartupTime {
		if _, exists := allowed[ip]; !exists {
			delete(instanceStartupTime, ip)
		}
	}
	stateMu.Unlock()
}

func getMonitoredIPsSnapshot() []string {
	monitoredIPsMu.RLock()
	defer monitoredIPsMu.RUnlock()
	return append([]string(nil), monitoredIPs...)
}

func getAppIPsFromTerraform() ([]string, error) {
	cmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("terraform output failed: %w", err)
	}

	var ips []string
	if err := json.Unmarshal(outBytes, &ips); err != nil {
		return nil, fmt.Errorf("failed to parse app_instance_ips: %w", err)
	}

	if len(ips) == 0 {
		return nil, errors.New("terraform returned zero app instance IPs")
	}

	return ips, nil
}

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
	// Request rate is NOT used for app instance scaling
	// All traffic routes through load balancer, so request metrics on instances are unreliable
	// Only CPU metrics determine scaling decisions for app instances
	log.Printf("[SCALING POLICY] Request rate monitoring disabled for app instances (traffic isolation enforced)")
	return 0, nil
}

func checkHealth(ips []string) map[string]bool {
	healthStatus := make(map[string]bool)
	client := http.Client{Timeout: 5 * time.Second}

	for _, ip := range ips {
		url := fmt.Sprintf("http://%s:8080/health", ip)
		resp, err := client.Get(url)
		
		stateMu.Lock()
		startupTime, hasStartupTime := instanceStartupTime[ip]
		isStartingUp := hasStartupTime && time.Since(startupTime) < InstanceStartupGracePeriod
		stateMu.Unlock()
		
		if err != nil {
			healthStatus[ip] = false
			stateMu.Lock()
			// During startup, don't increment failure count
			if !isStartingUp {
				healthCheckFailures[ip]++
				failureCount := healthCheckFailures[ip]
				stateMu.Unlock()
				log.Printf("[HEALTH CHECK FAIL] %s: %v (failure count: %d)", ip, err, failureCount)
			} else {
				stateMu.Unlock()
				// Suppress error logging during startup grace period
			}
			continue
		}

		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			healthStatus[ip] = true
			stateMu.Lock()
			healthCheckFailures[ip] = 0
			stateMu.Unlock()
			log.Printf("[HEALTH CHECK OK] %s: Healthy", ip)
		} else {
			healthStatus[ip] = false
			stateMu.Lock()
			// During startup, don't increment failure count
			if !isStartingUp {
				healthCheckFailures[ip]++
				failureCount := healthCheckFailures[ip]
				stateMu.Unlock()
				log.Printf("[HEALTH CHECK FAIL] %s: HTTP %d (failure count: %d)", ip, resp.StatusCode, failureCount)
			} else {
				stateMu.Unlock()
				// Suppress error logging during startup grace period
			}
		}
	}

	return healthStatus
}

func replaceUnhealthyInstance(ip string) {
	defer func() {
		stateMu.Lock()
		delete(replacementInProgress, ip)
		stateMu.Unlock()
	}()

	terraformOperationMu.Lock()
	defer terraformOperationMu.Unlock()

	appIPs, err := getAppIPsFromTerraform()
	if err != nil {
		log.Printf("[SELF-HEAL] Failed to read app IPs from Terraform before replacement: %v", err)
		return
	}

	instanceIndex := -1
	for i, candidate := range appIPs {
		if candidate == ip {
			instanceIndex = i
			break
		}
	}

	if instanceIndex == -1 {
		log.Printf("[SELF-HEAL] Instance IP %s no longer exists in Terraform output, skipping replace", ip)
		return
	}

	resourceRef := fmt.Sprintf("aws_instance.app_server[%d]", instanceIndex)
	log.Printf("[SELF-HEAL] Replacing unhealthy instance %s using %s", ip, resourceRef)

	cmd := exec.Command("terraform", "apply", "-replace", resourceRef, "-auto-approve")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Printf("[SELF-HEAL] Terraform replacement failed for %s: %v", ip, err)
		return
	}

	refreshedIPs, err := getAppIPsFromTerraform()
	if err != nil {
		log.Printf("[SELF-HEAL] Replacement succeeded but IP refresh failed: %v", err)
		return
	}

	setMonitoredIPs(refreshedIPs)
	log.Printf("[SELF-HEAL] Replacement completed for %s. Active app instances: %v", ip, refreshedIPs)
}

func runTerraform(count int) {
	terraformOperationMu.Lock()
	defer terraformOperationMu.Unlock()

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