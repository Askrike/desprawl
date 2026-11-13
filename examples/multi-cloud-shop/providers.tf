# One app spread over three providers: Auth0 signs users in, AWS holds the
# data and the storefront, and the API and workers run on Kubernetes (EKS).
terraform {
  required_providers {
    aws        = { source = "hashicorp/aws" }
    auth0      = { source = "auth0/auth0" }
    kubernetes = { source = "hashicorp/kubernetes" }
  }
}

provider "aws" {
  region = "eu-central-1"
}

provider "auth0" {}

provider "kubernetes" {
  host                   = aws_eks_cluster.main.endpoint
  cluster_ca_certificate = base64decode(aws_eks_cluster.main.certificate_authority[0].data)
}

variable "domain" {
  default = "shop.example.com"
}
