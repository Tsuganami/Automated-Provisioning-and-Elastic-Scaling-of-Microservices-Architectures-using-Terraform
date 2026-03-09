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

func main() {
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

	fmt.Println("=== Starting Continuous Stress Test (discovers new instances automatically) ===")
	stressedIPs := make(map[string]bool)
	go func() {
		for {
			currentIPs := getActiveIPs()
			for _, ip := range currentIPs {
				if stressedIPs[ip] {
					continue
				}
				fmt.Printf("Checking new instance %s... ", ip)
				_, err := fetchCPU(ip)
				if err != nil {
					fmt.Printf("NOT READY (%v)\n", err)
					continue
				}
				fmt.Println("OK — launching stress test")
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
