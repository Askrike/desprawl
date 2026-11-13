resource "kubernetes_namespace_v1" "shop" {
  metadata { name = "shop" }
}

resource "kubernetes_secret_v1" "worker" {
  metadata {
    name      = "worker-auth0"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  data = {
    client_id     = auth0_client.worker.client_id
    client_secret = auth0_client.worker.client_secret
  }
}

resource "kubernetes_deployment_v1" "api" {
  metadata {
    name      = "api"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  spec {
    replicas = 3
    selector { match_labels = { app = "api" } }
    template {
      metadata { labels = { app = "api" } }
      spec {
        container {
          name  = "api"
          image = "ghcr.io/example/shop-api:1.4.2"
          env {
            name  = "AUTH0_AUDIENCE"
            value = auth0_resource_server.api.identifier
          }
          env {
            name  = "DATABASE_HOST"
            value = aws_db_instance.main.address
          }
          env {
            name  = "REDIS_HOST"
            value = aws_elasticache_replication_group.sessions.primary_endpoint_address
          }
          env {
            name  = "UPLOADS_BUCKET"
            value = aws_s3_bucket.uploads.bucket
          }
          env {
            name  = "ORDERS_QUEUE_URL"
            value = aws_sqs_queue.orders.url
          }
        }
      }
    }
  }
}

resource "kubernetes_service_v1" "api" {
  metadata {
    name      = "api"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  spec {
    selector = { app = kubernetes_deployment_v1.api.spec[0].template[0].metadata[0].labels.app }
    port {
      port        = 80
      target_port = 8080
    }
  }
}

resource "kubernetes_ingress_v1" "api" {
  metadata {
    name      = "api"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  spec {
    rule {
      host = "api.${var.domain}"
      http {
        path {
          path = "/"
          backend {
            service {
              name = kubernetes_service_v1.api.metadata[0].name
              port { number = 80 }
            }
          }
        }
      }
    }
  }
}

resource "kubernetes_deployment_v1" "worker" {
  metadata {
    name      = "order-worker"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  spec {
    replicas = 2
    selector { match_labels = { app = "order-worker" } }
    template {
      metadata { labels = { app = "order-worker" } }
      spec {
        container {
          name  = "worker"
          image = "ghcr.io/example/shop-worker:1.4.2"
          env {
            name  = "ORDERS_QUEUE_URL"
            value = aws_sqs_queue.orders.url
          }
          env {
            name  = "DATABASE_HOST"
            value = aws_db_instance.main.address
          }
          env {
            name  = "API_AUDIENCE"
            value = auth0_resource_server.api.identifier
          }
          env_from {
            secret_ref { name = kubernetes_secret_v1.worker.metadata[0].name }
          }
        }
      }
    }
  }
}

resource "kubernetes_cron_job_v1" "sales_report" {
  metadata {
    name      = "sales-report"
    namespace = kubernetes_namespace_v1.shop.metadata[0].name
  }
  spec {
    schedule = "0 6 * * *"
    job_template {
      metadata {}
      spec {
        template {
          metadata {}
          spec {
            container {
              name  = "report"
              image = "ghcr.io/example/shop-report:1.4.2"
              env {
                name  = "DATABASE_HOST"
                value = aws_db_instance.main.address
              }
              env {
                name  = "REPORTS_BUCKET"
                value = aws_s3_bucket.uploads.bucket
              }
            }
          }
        }
      }
    }
  }
}
