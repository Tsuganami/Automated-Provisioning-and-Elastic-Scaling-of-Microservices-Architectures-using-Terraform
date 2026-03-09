provider "aws" {
  region = "us-east-1"
}

variable "instance_count" {
  description = "Number of microservice instances"
  type        = number
  default     = 1
}