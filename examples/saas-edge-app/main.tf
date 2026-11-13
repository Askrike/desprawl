variable "account_id" {}
variable "org" {}

resource "cloudflare_zone" "site" {
  account = { id = var.account_id }
  name    = "example.com"
}
resource "cloudflare_dns_record" "www" {
  zone_id = cloudflare_zone.site.id
  name    = "www"
  type    = "CNAME"
  content = vercel_project.web.name
  ttl     = 1
}
resource "cloudflare_r2_bucket" "uploads" {
  account_id = var.account_id
  name       = "uploads"
}
resource "cloudflare_queue" "jobs" {
  account_id = var.account_id
  queue_name = "jobs"
}
resource "cloudflare_workers_script" "api" {
  account_id  = var.account_id
  script_name = "api"
  bindings = [
    { type = "r2_bucket", name = "UPLOADS", bucket_name = cloudflare_r2_bucket.uploads.name },
    { type = "queue", name = "JOBS", queue_name = cloudflare_queue.jobs.queue_name },
    { type = "secret_text", name = "MONGO", text = mongodbatlas_advanced_cluster.main.connection_strings.standard_srv },
  ]
}
resource "cloudflare_workers_route" "api" {
  zone_id = cloudflare_zone.site.id
  pattern = "api.example.com/*"
  script  = cloudflare_workers_script.api.script_name
}
resource "cloudflare_workers_script" "worker" {
  account_id  = var.account_id
  script_name = "worker"
}
resource "cloudflare_queue_consumer" "jobs" {
  account_id  = var.account_id
  queue_id    = cloudflare_queue.jobs.queue_id
  script_name = cloudflare_workers_script.worker.script_name
}
resource "mongodbatlas_project" "p" {
  name   = "p"
  org_id = var.org
}
resource "mongodbatlas_advanced_cluster" "main" {
  project_id   = mongodbatlas_project.p.id
  name         = "main"
  cluster_type = "REPLICASET"
}
resource "mongodbatlas_database_user" "app" {
  project_id         = mongodbatlas_project.p.id
  username           = "app"
  auth_database_name = "admin"
}
resource "vercel_project" "web" {
  name = "web"
}
resource "vercel_project_environment_variable" "api" {
  project_id = vercel_project.web.id
  key        = "API_URL"
  value      = "https://api.example.com"
}
resource "vercel_blob_store" "assets" {
  name = "assets"
}
resource "vercel_blob_project_connection" "assets" {
  project_id    = vercel_project.web.id
  blob_store_id = vercel_blob_store.assets.id
}
