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
	ThresholdUp   = 70.0
	ThresholdDown = 30.0
	MinInstances  = 2
	MaxInstances  = 4
	Cooldown      = 3 * time.Minute
)

var currentInstances = 2
var lastScalingTime = time.Now().Add(-Cooldown)


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
		parts := strings.Split(string(body), " ")
		if len(parts) < 2 {
			log.Printf("Invalid metrics format from %s", ip)
			continue
		}
		val, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			log.Printf("Error parsing CPU value from %s: %v", ip, err)
			continue
		}
		totalCPU += val
		successCount++
	}
	if successCount == 0 {
		return 0, errors.New("could not retrieve data from any IP")
	}
	return totalCPU / float64(successCount), nil
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

	for {
		avgCPU, err := getAverageCPU(ips)
		if err != nil {
			log.Printf("Error: %v", err)
			time.Sleep(30 * time.Second)
			continue
		}
		fmt.Printf("Average CPU: %.2f%%\n", avgCPU)

		if avgCPU > ThresholdUp && currentInstances < MaxInstances {
			fmt.Println("Triggering Scale-up...")
			runTerraform(currentInstances + 1)
		} else if avgCPU < ThresholdDown && currentInstances > MinInstances {
			fmt.Println("Triggering Scale-down...")
			runTerraform(currentInstances - 1)
		}

		time.Sleep(30 * time.Second)
	}
}