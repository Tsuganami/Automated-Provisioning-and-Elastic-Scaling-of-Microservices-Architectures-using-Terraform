# Security group for Load Balancer - receives all external traffic
resource "aws_security_group" "lb_sg" {
  name        = "aps-load-balancer-sg"
  description = "Load Balancer - routes all traffic to app instances internally"

  # Accept HTTP traffic from external clients
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    description = "HTTP traffic from internet"
  }

  # Accept SSH for management (optional - remove in production)
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    description = "SSH for management"
  }

  # Allow all outbound traffic
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
    description = "All outbound traffic"
  }

  tags = {
    Name = "aps-load-balancer-sg"
  }
}

# Security group for App Servers - ISOLATED, no direct external traffic
resource "aws_security_group" "app_sg" {
  name        = "aps-app-servers-sg"
  description = "App servers - no direct external traffic, only LB communication"

  # Accept metrics/health checks ONLY from load balancer node on port 8080
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.lb_sg.id]
    description     = "Metrics and health endpoints ONLY from LB"
  }

  # Internal communication between app instances (for clustering if needed)
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    self            = true
    description     = "Internal app-to-app communication"
  }

  # SSH for management (optional - remove in production)
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
    description = "SSH for management"
  }

  # Block all other ingress traffic (implicit deny for security)
  # No ingress on 80 or any other port from external sources
  # No metrics endpoint exposed directly to internet

  # Allow all outbound traffic
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
    description = "All outbound traffic"
  }

  tags = {
    Name = "aps-app-servers-sg"
  }
}

# Legacy - keeping for backward compatibility (can be removed after migration)
resource "aws_security_group" "aps_sg" {
  name        = "aps-microservice-sg"
  description = "[DEPRECATED] Use lb_sg and app_sg instead"

  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    from_port   = 9090
    to_port     = 9090
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
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
}

# ============================================================================
# Encrypted Terraform State Storage (S3 Backend)
# ============================================================================

resource "aws_s3_bucket" "terraform_state" {
  bucket = "diploma-terraform-state-${data.aws_caller_identity.current.account_id}"

  tags = {
    Name        = "Terraform State Storage"
    Environment = "diploma"
    Purpose     = "Encrypted state backend"
  }
}

resource "aws_s3_bucket_versioning" "terraform_state" {
  bucket = aws_s3_bucket.terraform_state.id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "terraform_state" {
  bucket = aws_s3_bucket.terraform_state.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "terraform_state" {
  bucket = aws_s3_bucket.terraform_state.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# DynamoDB table for state locking (prevents concurrent operations)
resource "aws_dynamodb_table" "terraform_locks" {
  name           = "terraform-locks"
  billing_mode   = "PAY_PER_REQUEST"
  hash_key       = "LockID"

  attribute {
    name = "LockID"
    type = "S"
  }

  tags = {
    Name        = "Terraform State Locks"
    Environment = "diploma"
    Purpose     = "Lock table for concurrent state access"
  }
}