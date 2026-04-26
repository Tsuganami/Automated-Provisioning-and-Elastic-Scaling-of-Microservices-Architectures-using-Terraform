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
                            var spikeActive = false
                            var spikeMutex = &sync.Mutex{}

                            func incrementRequestCounter() {
                                    counterMutex.Lock()
                                    requestCounter++
                                    counterMutex.Unlock()
                            }

                            func burnCPU(d time.Duration) {
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
                                    time.Sleep(d)
                                    close(done)
                            }

                            func workHandler(w http.ResponseWriter, r *http.Request) {
                                    incrementRequestCounter()
                                    burnCPU(30 * time.Second)
                                    fmt.Fprintf(w, "Work finished on %s", os.Getenv("HOSTNAME"))
                            }

                            func requestHandler(w http.ResponseWriter, r *http.Request) {
                                    incrementRequestCounter()
                                    w.WriteHeader(http.StatusOK)
                                    fmt.Fprint(w, "request accepted")
                            }

                            // /spike triggers a sustained CPU spike that makes /health time out
                            // (single-threaded http server saturated by busy goroutines).
                            func spikeHandler(w http.ResponseWriter, r *http.Request) {
                                    spikeMutex.Lock()
                                    if spikeActive {
                                            spikeMutex.Unlock()
                                            fmt.Fprint(w, "spike already active")
                                            return
                                    }
                                    spikeActive = true
                                    spikeMutex.Unlock()

                                    fmt.Fprintf(w, "spike started on %s", os.Getenv("HOSTNAME"))

                                    go func() {
                                            // Far more goroutines than cores -> health checks start failing.
                                            workers := runtime.NumCPU() * 8
                                            done := make(chan int)
                                            for i := 0; i < workers; i++ {
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
                                            time.Sleep(5 * time.Minute)
                                            close(done)
                                            spikeMutex.Lock()
                                            spikeActive = false
                                            spikeMutex.Unlock()
                                    }()
                            }

                            func networkBytes() float64 {
                                    data, err := os.ReadFile("/proc/net/dev")
                                    if err != nil {
                                            return 0
                                    }
                                    total := 0.0
                                    for _, line := range strings.Split(string(data), "\n") {
                                            line = strings.TrimSpace(line)
                                            if !strings.Contains(line, ":") {
                                                    continue
                                            }
                                            parts := strings.Fields(line)
                                            if len(parts) < 10 {
                                                    continue
                                            }
                                            iface := strings.TrimSuffix(parts[0], ":")
                                            if iface == "lo" {
                                                    continue
                                            }
                                            rx, _ := strconv.ParseFloat(parts[1], 64)
                                            tx, _ := strconv.ParseFloat(parts[9], 64)
                                            total += rx + tx
                                    }
                                    return total
                            }

                            func metricsHandler(w http.ResponseWriter, r *http.Request) {
                                    counterMutex.Lock()
                                    requests := requestCounter
                                    counterMutex.Unlock()

                                    data, _ := os.ReadFile("/proc/loadavg")
                                    parts := strings.Split(string(data), " ")
                                    load, _ := strconv.ParseFloat(parts[0], 64)
                                    cpuPercent := (load / float64(runtime.NumCPU())) * 100

                                    netBytes := networkBytes()

                                    w.Header().Set("Content-Type", "text/plain; version=0.0.4")
                                    fmt.Fprintf(w, "# HELP cpu_utilization CPU utilization percentage\n")
                                    fmt.Fprintf(w, "# TYPE cpu_utilization gauge\n")
                                    fmt.Fprintf(w, "cpu_utilization %.2f\n", cpuPercent)
                                    fmt.Fprintf(w, "# HELP request_count Total number of requests processed\n")
                                    fmt.Fprintf(w, "# TYPE request_count counter\n")
                                    fmt.Fprintf(w, "request_count %d\n", requests)
                                    fmt.Fprintf(w, "# HELP network_bytes_total Total bytes in+out across non-loopback interfaces\n")
                                    fmt.Fprintf(w, "# TYPE network_bytes_total counter\n")
                                    fmt.Fprintf(w, "network_bytes_total %.0f\n", netBytes)
                            }

                            func healthHandler(w http.ResponseWriter, r *http.Request) {
                                    w.Header().Set("Content-Type", "application/json")
                                    w.WriteHeader(http.StatusOK)
                                    fmt.Fprintf(w, "{\"status\":\"healthy\",\"hostname\":\"%s\"}", os.Getenv("HOSTNAME"))
                            }

                            func main() {
                                    http.HandleFunc("/work", workHandler)
                                    http.HandleFunc("/request", requestHandler)
                                    http.HandleFunc("/spike", spikeHandler)
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
                                                        sudo apt-get install -y nginx python3

                                                        # Raise OS + nginx limits so synchronized bursts don't overflow the accept queue
                                                        echo "net.core.somaxconn = 4096" | sudo tee /etc/sysctl.d/99-aps.conf
                                                        echo "net.ipv4.tcp_max_syn_backlog = 4096" | sudo tee -a /etc/sysctl.d/99-aps.conf
                                                        sudo sysctl --system >/dev/null 2>&1 || true

                                                        cat << 'NGINXMAINEOF' > /etc/nginx/nginx.conf
                                                        user www-data;
                                                        worker_processes auto;
                                                        worker_rlimit_nofile 65535;
                                                        pid /run/nginx.pid;
                                                        include /etc/nginx/modules-enabled/*.conf;

                                                        events {
                                                                worker_connections 4096;
                                                                multi_accept on;
                                                                use epoll;
                                                        }

                                                        http {
                                                                sendfile on;
                                                                tcp_nopush on;
                                                                tcp_nodelay on;
                                                                keepalive_timeout 65;
                                                                types_hash_max_size 2048;
                                                                include /etc/nginx/mime.types;
                                                                default_type application/octet-stream;
                                                                access_log off;
                                                                error_log /var/log/nginx/error.log warn;
                                                                gzip on;
                                                                include /etc/nginx/conf.d/*.conf;
                                                                include /etc/nginx/sites-enabled/*;
                                                        }
                                                        NGINXMAINEOF

                                                        cat << 'NGINXEOF' > /etc/nginx/sites-available/default
                                                        # Placeholder upstream - the real backend list is pushed at runtime
                                                        # by lb_control.py (port 9999) on every scale/heal event. This block
                                                        # exists so nginx can start cleanly before the first POST /reload.
                                                        upstream app_backend {
                                                                server 127.0.0.1:8080 max_fails=1 fail_timeout=2s;
                                                                keepalive 32;
                                                        }

                                                        server {
                                                                listen 80 default_server backlog=4096;
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

                                                        # --- LB control agent: rewrites upstream and reloads nginx in-place ---
                                                        cat << 'PYEOF' > /opt/lb_control.py
                                                        #!/usr/bin/env python3
                                                        import subprocess
                                                        from http.server import BaseHTTPRequestHandler, HTTPServer
                                                        from urllib.parse import urlparse, parse_qs

                                                        AUTH = "aps-control-2026"

                                                        TEMPLATE = """upstream app_backend {{
                                                        {servers}    keepalive 32;
                                                        }}

                                                        server {{
                                                            listen 80 default_server backlog=4096;
                                                            server_name _;

                                                            location / {{
                                                                proxy_set_header Host $host;
                                                                proxy_set_header X-Real-IP $remote_addr;
                                                                proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
                                                                proxy_set_header X-Forwarded-Proto $scheme;
                                                                proxy_http_version 1.1;
                                                                proxy_set_header Connection "";
                                                                proxy_pass http://app_backend;
                                                            }}
                                                        }}
                                                        """

                                                        class H(BaseHTTPRequestHandler):
                                                            def do_POST(self):
                                                                u = urlparse(self.path)
                                                                q = parse_qs(u.query)
                                                                if q.get("token", [""])[0] != AUTH:
                                                                    self.send_response(401); self.end_headers(); return
                                                                if u.path != "/reload":
                                                                    self.send_response(404); self.end_headers(); return
                                                                ips = [x.strip() for x in q.get("ips", [""])[0].split(",") if x.strip()]
                                                                if not ips:
                                                                    self.send_response(400); self.end_headers(); self.wfile.write(b"no ips"); return
                                                                servers = "".join("    server %s:8080 max_fails=1 fail_timeout=2s;\n" % ip for ip in ips)
                                                                cfg = TEMPLATE.format(servers=servers)
                                                                try:
                                                                    with open("/etc/nginx/sites-available/default", "w") as f:
                                                                        f.write(cfg)
                                                                    r = subprocess.run(["nginx", "-s", "reload"], capture_output=True, timeout=10)
                                                                    if r.returncode != 0:
                                                                        self.send_response(500); self.end_headers(); self.wfile.write(r.stderr); return
                                                                except Exception as e:
                                                                    self.send_response(500); self.end_headers(); self.wfile.write(str(e).encode()); return
                                                                self.send_response(200); self.end_headers()
                                                                self.wfile.write(("reloaded with %d backend(s)" % len(ips)).encode())
                                                            def log_message(self, *a):
                                                                pass

                                                        HTTPServer(("0.0.0.0", 9999), H).serve_forever()
                                                        PYEOF
                                                        chmod +x /opt/lb_control.py

                                                        cat << 'SVCEOF' > /etc/systemd/system/lbcontrol.service
                                                        [Unit]
                                                        Description=LB Control Agent
                                                        After=network.target nginx.service

                                                        [Service]
                                                        ExecStart=/usr/bin/python3 /opt/lb_control.py
                                                        Restart=always
                                                        RestartSec=3
                                                        User=root

                                                        [Install]
                                                        WantedBy=multi-user.target
                                                        SVCEOF

                                                        systemctl daemon-reload
                                                        systemctl enable lbcontrol.service
                                                        systemctl start lbcontrol.service
                                                        EOF
}

resource "aws_instance" "lb_sender" {
    ami           = "ami-0c7217cdde317cfec"
    instance_type = "t3.micro"

        vpc_security_group_ids = [local.aps_lb_sg_id]
        user_data              = local.lb_user_data

        depends_on = [aws_instance.app_server]

    tags = {
        Name    = "APS-LB-Sender"
        Project = "Diploma"
        Role    = "load-balancer"
    }

    # The LB user_data installs nginx + the lb_control.py agent. Backend
    # IPs are NOT embedded - they are pushed at runtime by the scaler via
    # POST :9999/reload. Therefore user_data is stable across scaling
    # events, and we explicitly tell terraform to never replace the LB on
    # user_data drift. This prevents the LB from being destroyed/recreated
    # during a scale-up (which previously caused a brief outage and a new
    # public IP every time we scaled).
    lifecycle {
        ignore_changes        = [user_data]
        create_before_destroy = true
    }
}

resource "aws_instance" "app_server" {
    count         = var.app_instance_count
    ami           = "ami-0c7217cdde317cfec"
    instance_type = "t3.micro"

                vpc_security_group_ids = [local.aps_app_sg_id]
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

output "app_instance_private_ips" {
    value = aws_instance.app_server.*.private_ip
}

output "instance_ips" {
    value = concat([aws_instance.lb_sender.public_ip], aws_instance.app_server.*.public_ip)
}