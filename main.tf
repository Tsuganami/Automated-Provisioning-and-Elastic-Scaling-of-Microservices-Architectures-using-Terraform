resource "aws_instance" "app_server" {
  count         = var.instance_count
  ami           = "ami-0c7217cdde317cfec" 
  instance_type = "t3.micro"

  vpc_security_group_ids = [aws_security_group.aps_sg.id]

  
  user_data = <<-EOF
              #!/bin/bash
              sudo apt-get update
              sudo apt-get install -y golang-go
              
            
              cat << 'GOEOF' > /home/ubuntu/main.go
              package main

              import (
                  "fmt"
                  "net/http"
                  "os"
                  "runtime"
                  "strconv"
                  "strings"
                  "sync"
                  "time"
              )
              
              var requestCounter = 0
              var counterMutex = &sync.Mutex{}

              func workHandler(w http.ResponseWriter, r *http.Request) {
                  counterMutex.Lock()
                  requestCounter++
                  counterMutex.Unlock()
                  
                  done := make(chan int)
                  for i := 0; i < runtime.NumCPU(); i++ {
                      go func() {
                          for {
                              select {
                              case <-done:
                                  return
                              default:
                              }
                          }
                      }()
                  }
                  time.Sleep(30 * time.Second)
                  close(done)
                  fmt.Fprintf(w, "Work finished on %s", os.Getenv("HOSTNAME"))
              }

              func metricsHandler(w http.ResponseWriter, r *http.Request) {
                  counterMutex.Lock()
                  requests := requestCounter
                  counterMutex.Unlock()
                  
                  data, _ := os.ReadFile("/proc/loadavg")
                  parts := strings.Split(string(data), " ")
                  load, _ := strconv.ParseFloat(parts[0], 64)
                  cpuPercent := (load / float64(runtime.NumCPU())) * 100
                  
                  // REQ-6.1: Return metrics in Prometheus format
                  w.Header().Set("Content-Type", "text/plain; version=0.0.4")
                  fmt.Fprintf(w, "# HELP cpu_utilization CPU utilization percentage\n")
                  fmt.Fprintf(w, "# TYPE cpu_utilization gauge\n")
                  fmt.Fprintf(w, "cpu_utilization %.2f\n", cpuPercent)
                  fmt.Fprintf(w, "# HELP request_count Total number of requests processed\n")
                  fmt.Fprintf(w, "# TYPE request_count counter\n")
                  fmt.Fprintf(w, "request_count %d\n", requests)
              }
              
              // REQ-5.1: Health check endpoint
              func healthHandler(w http.ResponseWriter, r *http.Request) {
                  w.Header().Set("Content-Type", "application/json")
                  w.WriteHeader(http.StatusOK)
                  fmt.Fprintf(w, "{\"status\":\"healthy\",\"hostname\":\"%s\"}", os.Getenv("HOSTNAME"))
              }

              func main() {
                  http.HandleFunc("/work", workHandler)
                  http.HandleFunc("/metrics", metricsHandler)
                  http.HandleFunc("/health", healthHandler)  // REQ-5.1
                  http.ListenAndServe(":8080", nil)
              }
              GOEOF

              
              cat << 'SVCEOF' > /etc/systemd/system/goapp.service
              [Unit]
              Description=Go Metrics App
              After=network.target

              [Service]
              ExecStart=/usr/bin/go run /home/ubuntu/main.go
              Restart=always
              RestartSec=5
              Environment=HOME=/home/ubuntu

              [Install]
              WantedBy=multi-user.target
              SVCEOF

              systemctl daemon-reload
              systemctl enable goapp.service
              systemctl start goapp.service
              EOF

  tags = {
    Name    = "APS-Microservice-${count.index}"
    Project = "Diploma"
  }
}

output "instance_ips" {
  value = aws_instance.app_server.*.public_ip
}