locals {
    app_user_data = <<-EOF
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

                            func incrementRequestCounter() {
                                    counterMutex.Lock()
                                    requestCounter++
                                    counterMutex.Unlock()
                            }

                            func workHandler(w http.ResponseWriter, r *http.Request) {
                                    incrementRequestCounter()

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

                            func requestHandler(w http.ResponseWriter, r *http.Request) {
                                    incrementRequestCounter()
                                    w.WriteHeader(http.StatusOK)
                                    fmt.Fprint(w, "request accepted")
                            }

                            func metricsHandler(w http.ResponseWriter, r *http.Request) {
                                    counterMutex.Lock()
                                    requests := requestCounter
                                    counterMutex.Unlock()

                                    data, _ := os.ReadFile("/proc/loadavg")
                                    parts := strings.Split(string(data), " ")
                                    load, _ := strconv.ParseFloat(parts[0], 64)
                                    cpuPercent := (load / float64(runtime.NumCPU())) * 100

                                    w.Header().Set("Content-Type", "text/plain; version=0.0.4")
                                    fmt.Fprintf(w, "# HELP cpu_utilization CPU utilization percentage\n")
                                    fmt.Fprintf(w, "# TYPE cpu_utilization gauge\n")
                                    fmt.Fprintf(w, "cpu_utilization %.2f\n", cpuPercent)
                                    fmt.Fprintf(w, "# HELP request_count Total number of requests processed\n")
                                    fmt.Fprintf(w, "# TYPE request_count counter\n")
                                    fmt.Fprintf(w, "request_count %d\n", requests)
                            }

                            func healthHandler(w http.ResponseWriter, r *http.Request) {
                                    w.Header().Set("Content-Type", "application/json")
                                    w.WriteHeader(http.StatusOK)
                                    fmt.Fprintf(w, "{\"status\":\"healthy\",\"hostname\":\"%s\"}", os.Getenv("HOSTNAME"))
                            }

                            func main() {
                                    http.HandleFunc("/work", workHandler)
                                    http.HandleFunc("/request", requestHandler)
                                    http.HandleFunc("/metrics", metricsHandler)
                                    http.HandleFunc("/health", healthHandler)
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

        lb_user_data = <<-EOF
                                                        #!/bin/bash
                                                        sudo apt-get update
                                                        sudo apt-get install -y nginx

                                                        cat << 'NGINXEOF' > /etc/nginx/sites-available/default
                                                        upstream app_backend {
                                                        %{ for ip in aws_instance.app_server[*].private_ip ~}
                                                                server ${ip}:8080 max_fails=3 fail_timeout=10s;
                                                        %{ endfor ~}
                                                                keepalive 32;
                                                        }

                                                        server {
                                                                listen 80 default_server;
                                                                server_name _;

                                                                location / {
                                                                        proxy_set_header Host $host;
                                                                        proxy_set_header X-Real-IP $remote_addr;
                                                                        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
                                                                        proxy_set_header X-Forwarded-Proto $scheme;
                                                                        proxy_http_version 1.1;
                                                                        proxy_set_header Connection "";
                                                                        proxy_pass http://app_backend;
                                                                }
                                                        }
                                                        NGINXEOF

                                                        sudo nginx -t
                                                        sudo systemctl enable nginx
                                                        sudo systemctl restart nginx
                                                        EOF
}

resource "aws_instance" "lb_sender" {
    ami           = "ami-0c7217cdde317cfec"
    instance_type = "t3.micro"

    vpc_security_group_ids = [aws_security_group.aps_sg.id]
        user_data              = local.lb_user_data
        user_data_replace_on_change = true

        depends_on = [aws_instance.app_server]

    tags = {
        Name    = "APS-LB-Sender"
        Project = "Diploma"
        Role    = "load-balancer"
    }
}

resource "aws_instance" "app_server" {
    count         = var.app_instance_count
    ami           = "ami-0c7217cdde317cfec"
    instance_type = "t3.micro"

    vpc_security_group_ids = [aws_security_group.aps_sg.id]
    user_data              = local.app_user_data

    tags = {
        Name    = "APS-App-${count.index}"
        Project = "Diploma"
        Role    = "app"
    }
}

output "lb_ip" {
    value = aws_instance.lb_sender.public_ip
}

output "app_instance_ips" {
    value = aws_instance.app_server.*.public_ip
}

output "instance_ips" {
    value = concat([aws_instance.lb_sender.public_ip], aws_instance.app_server.*.public_ip)
}