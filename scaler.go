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
	ThresholdUp			= 50.0
	ThresholdDown			= 25.0
	MinInstances			= 1
	MaxInstances			= 3
	Cooldown			= 30 * time.Second
	MonitoringInterval		= 10 * time.Second
	ScaleUpTimeWindow		= 15 * time.Second
	ScaleDownTimeWindow		= 60 * time.Second
	RequestRateThresholdUp		= 40.0
	RequestRateThresholdDown	= 20.0
	HealthCheckInterval		= 10 * time.Second

	UnhealthyCPUThreshold	= 60.0

	UnhealthyChecksRequired	= 3

	HealReadinessTimeout	= 5 * time.Minute

	HealReadinessPollInterval	= 5 * time.Second

	HealDrainGracePeriod	= 5 * time.Second
)

var (
	currentInstances	= 1
	lastScalingTime		= time.Now().Add(-Cooldown)
	previousRequestCounts	= make(map[string]float64)
	thresholdExceededTime	time.Time
	thresholdDroppedTime	time.Time
	highCPUSamples		= make(map[string]int)
	tfMu			sync.Mutex
	replacingMu		sync.Mutex
	replacing		= make(map[string]bool)
)

func getAverageCPU(ips []string) (float64, error) {
	var totalCPU float64
	var successCount int
	client := http.Client{Timeout: 5 * time.Second}
	for _, ip := range ips {
		resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
		if err != nil {
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if strings.HasPrefix(line, "cpu_utilization ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					if val, err := strconv.ParseFloat(parts[1], 64); err == nil {
						totalCPU += val
						successCount++
					}
				}
				break
			}
		}
	}
	if successCount == 0 {
		return 0, errors.New("could not retrieve cpu from any IP")
	}
	return totalCPU / float64(successCount), nil
}

func getTotalRequestRate(ips []string) (float64, error) {
	var totalRate float64
	var successCount int
	client := http.Client{Timeout: 5 * time.Second}

	for _, ip := range ips {
		resp, err := client.Get(fmt.Sprintf("http://%s:8080/metrics", ip))
		if err != nil {
			continue
		}
		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		var requestCount float64
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if strings.HasPrefix(line, "request_count ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					if val, perr := strconv.ParseFloat(parts[1], 64); perr == nil {
						requestCount = val
					}
				}
				break
			}
		}
		if previous, exists := previousRequestCounts[ip]; exists {
			elapsed := MonitoringInterval.Seconds()
			delta := requestCount - previous
			if delta < 0 {
				delta = 0
			}
			totalRate += delta / elapsed
		}
		previousRequestCounts[ip] = requestCount
		successCount++
	}
	if successCount == 0 {
		return 0, errors.New("could not retrieve request rate from any IP")
	}
	return totalRate, nil
}

func checkHealth(ips []string) {
	client := http.Client{Timeout: 4 * time.Second}
	for _, ip := range ips {
		url := fmt.Sprintf("http://%s:8080/health", ip)
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("[HEALTH PING] %s: %v (informational only)", ip, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("[HEALTH PING] %s: HTTP %d (informational only)", ip, resp.StatusCode)
		}
	}
}

func fetchInstanceCPU(ip string) (float64, error) {
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
		if strings.HasPrefix(line, "cpu_utilization ") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, errors.New("malformed cpu line")
			}
			return strconv.ParseFloat(parts[1], 64)
		}
	}
	return 0, errors.New("cpu_utilization not found")
}

func runTerraformScale(count int) {
	tfMu.Lock()
	if time.Since(lastScalingTime) < Cooldown {
		tfMu.Unlock()
		fmt.Println("Cooldown active, skipping scale...")
		return
	}

	if count < currentInstances {
		privs := getAppPrivateIPs()
		if len(privs) >= count && count > 0 {
			drained := privs[:count]
			log.Printf("[SCALE-DOWN] Pre-draining LB to %d backend(s) before destroy", len(drained))
			notifyLBWithIPs(drained)

			time.Sleep(2 * time.Second)
		}

		pubs := getActiveAppIPs()
		if len(pubs) > count {
			for _, ip := range pubs[count:] {
				notifyDashboardRemoved(ip)
			}
		}
	}

	fmt.Printf("--- TERRAFORM APPLY (target instances: %d) ---\n", count)
	cmd := exec.Command("terraform", "apply",
		"-var", fmt.Sprintf("app_instance_count=%d", count),
		"-auto-approve", "-lock=false")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		tfMu.Unlock()
		fmt.Printf("Terraform failed: %v\n", err)
		return
	}
	currentInstances = count
	lastScalingTime = time.Now()
	fmt.Printf("--- SUCCESS: Scaled to %d ---\n", count)

	tfMu.Unlock()
	go notifyLB()
}

func notifyDashboardRemoved(ip string) {
	url := fmt.Sprintf("http://localhost:9090/api/instance-removed?ip=%s", ip)
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(url, "application/json", nil)
	if err != nil {
		log.Printf("[DASHBOARD-REMOVE] Could not notify dashboard about %s: %v", ip, err)
		return
	}
	resp.Body.Close()
	log.Printf("[DASHBOARD-REMOVE] Notified dashboard: %s is being destroyed", ip)
}

func findInstanceIndex(ip string) (int, error) {
	cmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
	out, err := cmd.Output()
	if err != nil {
		return -1, err
	}
	var ips []string
	if err := json.Unmarshal(out, &ips); err != nil {
		return -1, err
	}
	for i, candidate := range ips {
		if candidate == ip {
			return i, nil
		}
	}
	return -1, fmt.Errorf("ip %s not found in terraform output", ip)
}

func replaceInstance(badIP string) {
	replacingMu.Lock()
	if replacing[badIP] {
		replacingMu.Unlock()
		return
	}
	replacing[badIP] = true
	replacingMu.Unlock()
	defer func() {
		replacingMu.Lock()
		delete(replacing, badIP)
		replacingMu.Unlock()
	}()

	badIdx, err := findInstanceIndex(badIP)
	if err != nil {
		log.Printf("[SELF-HEAL] Cannot locate %s in terraform state: %v", badIP, err)
		return
	}

	tfMu.Lock()
	defer tfMu.Unlock()

	oldCount := currentInstances
	canScaleUp := oldCount < MaxInstances

	if !canScaleUp {
		log.Printf("[SELF-HEAL] At MaxInstances (%d) - falling back to in-place replace of %s", MaxInstances, badIP)
		if err := tfApplyReplace(badIdx, oldCount); err != nil {
			log.Printf("[SELF-HEAL] In-place replace failed: %v", err)
			return
		}
		highCPUSamples[badIP] = 0
		delete(previousRequestCounts, badIP)
		lastScalingTime = time.Now()
		log.Printf("[SELF-HEAL] In-place replacement complete.")
		go notifyLB()
		return
	}

	newCount := oldCount + 1
	log.Printf("[SELF-HEAL] Provision-first heal of %s: scaling %d -> %d", badIP, oldCount, newCount)

	preIPs := getActiveAppIPs()
	preSet := make(map[string]bool, len(preIPs))
	for _, ip := range preIPs {
		preSet[ip] = true
	}

	if err := tfApplyScale(newCount); err != nil {
		log.Printf("[SELF-HEAL] Scale-up to %d failed: %v - aborting heal", newCount, err)
		return
	}
	currentInstances = newCount

	postIPs := getActiveAppIPs()
	var newIP string
	for _, ip := range postIPs {
		if !preSet[ip] {
			newIP = ip
			break
		}
	}
	if newIP == "" {
		log.Printf("[SELF-HEAL] Could not identify newly-provisioned instance - aborting heal")
		return
	}
	log.Printf("[SELF-HEAL] New replacement instance is %s - waiting up to %v for it to become ready", newIP, HealReadinessTimeout)

	if !waitForInstanceReady(newIP, HealReadinessTimeout) {
		log.Printf("[SELF-HEAL] New instance %s did not become ready within %v - aborting destroy of bad instance %s", newIP, HealReadinessTimeout, badIP)

		lastScalingTime = time.Now()
		go notifyLB()
		return
	}
	log.Printf("[SELF-HEAL] New instance %s is READY", newIP)

	drainedIPs := privateIPsExcluding(badIP)
	if len(drainedIPs) > 0 {
		log.Printf("[SELF-HEAL] Draining %s from LB (%d backend(s) remain)", badIP, len(drainedIPs))
		notifyLBWithIPs(drainedIPs)
	} else {
		log.Printf("[SELF-HEAL] Refusing to drain %s - it would leave LB with 0 backends", badIP)
	}

	time.Sleep(HealDrainGracePeriod)

	log.Printf("[SELF-HEAL] Destroying bad instance %s (aws_instance.app_server[%d])", badIP, badIdx)
	if err := tfApplyReplace(badIdx, newCount); err != nil {
		log.Printf("[SELF-HEAL] Destroy of bad instance failed: %v", err)

		go notifyLBWithIPs(drainedIPs)
		return
	}
	highCPUSamples[badIP] = 0
	delete(previousRequestCounts, badIP)
	lastScalingTime = time.Now()

	log.Printf("[SELF-HEAL] Heal complete. Final instance count: %d", currentInstances)
	go notifyLB()
}

func tfApplyScale(count int) error {
	cmd := exec.Command("terraform", "apply",
		"-var", fmt.Sprintf("app_instance_count=%d", count),
		"-auto-approve", "-lock=false")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func tfApplyReplace(idx, count int) error {
	target := fmt.Sprintf("aws_instance.app_server[%d]", idx)
	cmd := exec.Command("terraform", "apply",
		"-replace", target,
		"-var", fmt.Sprintf("app_instance_count=%d", count),
		"-auto-approve", "-lock=false")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForInstanceReady(ip string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := fetchInstanceCPU(ip); err == nil {
			return true
		}
		time.Sleep(HealReadinessPollInterval)
	}
	return false
}

func privateIPsExcluding(excludePublicIP string) []string {
	pubs := getActiveAppIPs()
	privs := getAppPrivateIPs()
	if len(pubs) != len(privs) {

		return privs
	}
	out := make([]string, 0, len(privs))
	for i, p := range pubs {
		if p == excludePublicIP {
			continue
		}
		out = append(out, privs[i])
	}
	return out
}

func getActiveAppIPs() []string {
	cmd := exec.Command("terraform", "output", "-json", "app_instance_ips")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var ips []string
	if err := json.Unmarshal(out, &ips); err != nil {
		return nil
	}
	return ips
}

func getAppPrivateIPs() []string {
	cmd := exec.Command("terraform", "output", "-json", "app_instance_private_ips")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var ips []string
	if err := json.Unmarshal(out, &ips); err != nil {
		return nil
	}
	return ips
}

func getLBPublicIP() string {
	cmd := exec.Command("terraform", "output", "-raw", "lb_ip")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func notifyLB() {
	notifyLBWithIPs(getAppPrivateIPs())
}

func notifyLBWithIPs(privateIPs []string) {
	lbIP := getLBPublicIP()
	if lbIP == "" || len(privateIPs) == 0 {
		log.Printf("[LB-RELOAD] Skipped: lb_ip=%q, private_ips=%v", lbIP, privateIPs)
		return
	}
	url := fmt.Sprintf("http://%s:9999/reload?token=aps-control-2026&ips=%s",
		lbIP, strings.Join(privateIPs, ","))
	client := &http.Client{Timeout: 8 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= 6; attempt++ {
		resp, err := client.Post(url, "application/json", nil)
		if err == nil && resp.StatusCode == 200 {
			body, _ := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			log.Printf("[LB-RELOAD] OK (%d backends): %s", len(privateIPs), strings.TrimSpace(string(body)))
			return
		}
		if resp != nil {
			resp.Body.Close()
		}
		lastErr = err
		log.Printf("[LB-RELOAD] attempt %d/6 failed: %v", attempt, err)
		if attempt < 6 {
			time.Sleep(3 * time.Second)
		}
	}
	log.Printf("[LB-RELOAD] FAILED after retries: %v", lastErr)
	log.Printf("[LB-RELOAD] >>> Traffic will NOT be diverted to new instances. " +
		"Check that the LB security group allows inbound TCP/9999 and that " +
		"the lb_control.py systemd service is running on the LB host.")
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: scaler <ip1> <ip2> ...")
	}

	currentInstances = len(os.Args[1:])
	fmt.Printf("Initial instance count: %d\n", currentInstances)

	go func() {
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			privateIPs := getAppPrivateIPs()
			lbIP := getLBPublicIP()
			if lbIP != "" && len(privateIPs) > 0 {
				url := fmt.Sprintf("http://%s:9999/reload?token=aps-control-2026&ips=%s",
					lbIP, strings.Join(privateIPs, ","))
				client := &http.Client{Timeout: 8 * time.Second}
				resp, err := client.Post(url, "application/json", nil)
				if err == nil && resp.StatusCode == 200 {
					resp.Body.Close()
					log.Printf("[LB-INIT] Initial backend list pushed (%d backend(s))", len(privateIPs))
					return
				}
				if resp != nil {
					resp.Body.Close()
				}
				log.Printf("[LB-INIT] LB control agent not ready yet: %v - retrying", err)
			}
			time.Sleep(5 * time.Second)
		}
		log.Printf("[LB-INIT] Gave up after 5m - LB may still be running placeholder upstream")
	}()

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/replace", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			ip := strings.TrimSpace(r.URL.Query().Get("ip"))
			if ip == "" {
				http.Error(w, "missing ip query parameter", http.StatusBadRequest)
				return
			}

			known := false
			for _, candidate := range getActiveAppIPs() {
				if candidate == ip {
					known = true
					break
				}
			}
			if !known {
				http.Error(w, "ip is not an active app instance", http.StatusBadRequest)
				return
			}

			replacingMu.Lock()
			alreadyReplacing := replacing[ip]
			replacingMu.Unlock()
			if alreadyReplacing {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"status":	"already_replacing",
					"ip":		ip,
				})
				return
			}

			drained := privateIPsExcluding(ip)
			if len(drained) > 0 {
				log.Printf("[FAIL-HEALTH] Manual fail on %s - draining LB to %d backend(s) immediately",
					ip, len(drained))
				go notifyLBWithIPs(drained)
			} else {
				log.Printf("[FAIL-HEALTH] Manual fail on %s - cannot drain (would leave 0 backends), will still replace", ip)
			}

			log.Printf("[FAIL-HEALTH] Marking %s for replacement", ip)
			go replaceInstance(ip)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":	"replacing",
				"ip":		ip,
				"drained":	len(drained),
			})
		})
		if err := http.ListenAndServe("127.0.0.1:9091", mux); err != nil {
			log.Printf("[SCALER-CTL] HTTP server failed: %v", err)
		}
	}()

	go func() {
		ticker := time.NewTicker(HealthCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			ips := getActiveAppIPs()
			if len(ips) == 0 {
				continue
			}

			checkHealth(ips)

			active := make(map[string]bool, len(ips))
			for _, ip := range ips {
				active[ip] = true
				cpu, err := fetchInstanceCPU(ip)
				if err != nil {

					log.Printf("[CPU CHECK] %s: unable to read CPU (%v) - skipping", ip, err)
					continue
				}
				if cpu > UnhealthyCPUThreshold {
					highCPUSamples[ip]++
					log.Printf("[CPU CHECK] %s: CPU=%.1f%% > %.1f%% (sample %d/%d)",
						ip, cpu, UnhealthyCPUThreshold, highCPUSamples[ip], UnhealthyChecksRequired)
					if highCPUSamples[ip] >= UnhealthyChecksRequired {
						log.Printf("[SELF-HEAL] %s sustained CPU > %.1f%% for %d samples - replacing",
							ip, UnhealthyCPUThreshold, UnhealthyChecksRequired)
						go replaceInstance(ip)
					}
				} else {
					if highCPUSamples[ip] > 0 {
						log.Printf("[CPU CHECK] %s: CPU=%.1f%% recovered, clearing counter", ip, cpu)
					}
					highCPUSamples[ip] = 0
				}
			}

			for ip := range highCPUSamples {
				if !active[ip] {
					delete(highCPUSamples, ip)
				}
			}
		}
	}()

	for {
		ips := getActiveAppIPs()
		if len(ips) == 0 {
			time.Sleep(MonitoringInterval)
			continue
		}
		currentInstances = len(ips)

		avgCPU, _ := getAverageCPU(ips)
		totalRPS, _ := getTotalRequestRate(ips)

		fmt.Printf("=== METRICS [%s] CPU=%.1f%% TotalRPS=%.1f Instances=%d ===\n",
			time.Now().Format("15:04:05"), avgCPU, totalRPS, currentInstances)

		perInstanceRate := 0.0
		if currentInstances > 0 {
			perInstanceRate = totalRPS / float64(currentInstances)
		}

		scaleUp := avgCPU > ThresholdUp || perInstanceRate > RequestRateThresholdUp
		if scaleUp {
			if thresholdExceededTime.IsZero() {
				thresholdExceededTime = time.Now()
				log.Printf("[SCALE-UP] Threshold breached - waiting %v", ScaleUpTimeWindow)
			}
			if time.Since(thresholdExceededTime) >= ScaleUpTimeWindow && currentInstances < MaxInstances {
				fmt.Printf(">>> SCALE-UP: cpu=%.1f rate/inst=%.1f\n", avgCPU, perInstanceRate)
				runTerraformScale(currentInstances + 1)
				thresholdExceededTime = time.Time{}
			}
		} else {
			thresholdExceededTime = time.Time{}
		}

		scaleDown := avgCPU < ThresholdDown || perInstanceRate < RequestRateThresholdDown
		if scaleDown && currentInstances > MinInstances {
			if thresholdDroppedTime.IsZero() {
				thresholdDroppedTime = time.Now()
				log.Printf("[SCALE-DOWN] Below threshold - waiting %v", ScaleDownTimeWindow)
			}
			if time.Since(thresholdDroppedTime) >= ScaleDownTimeWindow {
				fmt.Printf(">>> SCALE-DOWN: cpu=%.1f rate/inst=%.1f\n", avgCPU, perInstanceRate)
				runTerraformScale(currentInstances - 1)
				thresholdDroppedTime = time.Time{}
			}
		} else {
			thresholdDroppedTime = time.Time{}
		}

		time.Sleep(MonitoringInterval)
	}
}
