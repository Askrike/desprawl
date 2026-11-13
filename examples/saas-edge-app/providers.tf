# An app with no hyperscaler: Vercel serves the frontend, Cloudflare runs the
# API and a queue worker at the edge with R2 storage, and MongoDB Atlas holds
# the data.
terraform {
  required_providers {
    cloudflare   = { source = "cloudflare/cloudflare" }
    vercel       = { source = "vercel/vercel" }
    mongodbatlas = { source = "mongodb/mongodbatlas" }
  }
}

provider "cloudflare" {}
provider "vercel" {}
provider "mongodbatlas" {}
