terraform {
  backend "s3" {
    bucket         = "diploma-terraform-state-593710895467"
    key            = "terraform.tfstate"
    region         = "us-east-1"
    encrypt        = true
    use_lockfile   = true
  }
}

provider "aws" {
  region = "us-east-1"
}

data "aws_caller_identity" "current" {}

variable "app_instance_count" {
  description = "Number of scalable app instances (does not include fixed LB sender node)"
  type        = number
  default     = 1
}