data "aws_vpc" "default" {
  default = true
}

# NOTE: We intentionally do NOT reuse pre-existing security groups via
# `data "aws_security_groups"` anymore. Reusing an old SG that was created
# before port 9999 (lb_control.py) was added causes the dashboard's
# notifyLB() POSTs to be silently dropped at the SG, which in turn means
# newly-scaled instances are never registered with nginx and traffic isn't
# diverted.
#
# The SG names are bumped to "-v2" so a fresh group is provisioned cleanly.
# Any leftover "aps-microservice-sg" / "aps-app-sg" from a previous deployment
# can be deleted manually in the AWS console; nothing else references them.

resource "aws_security_group" "aps_lb_sg" {
  name        = "aps-microservice-sg-v2"
  description = "Allow public traffic to load balancer (HTTP + lb_control agent)"
  vpc_id      = data.aws_vpc.default.id

  ingress {
    description = "HTTP traffic"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "lb_control reload agent (used by scaler.notifyLB)"
    from_port   = 9999
    to_port     = 9999
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "aps_app_sg" {
  name        = "aps-app-sg-v2"
  description = "Allow app traffic from load balancer + metrics scrape"
  vpc_id      = data.aws_vpc.default.id

  # nginx -> app traffic
  ingress {
    description     = "App port from LB SG"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.aps_lb_sg.id]
  }

  # The Go scaler (running on the operator's machine) scrapes /metrics and
  # /health directly on each app instance. Without this rule the scaler can
  # never confirm a new instance is ready.
  ingress {
    description = "Metrics scrape from operator/scaler (public)"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  lifecycle {
    create_before_destroy = true
  }
}

locals {
  aps_lb_sg_id  = aws_security_group.aps_lb_sg.id
  aps_app_sg_id = aws_security_group.aps_app_sg.id
}
