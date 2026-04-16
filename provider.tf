provider "aws" {
  region = "us-east-1"
}

variable "app_instance_count" {
  description = "Number of scalable app instances (does not include fixed LB sender node)"
  type        = number
  default     = 1
}