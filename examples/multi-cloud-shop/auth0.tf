resource "auth0_resource_server" "api" {
  name       = "Shop API"
  identifier = "https://api.${var.domain}"
}

resource "auth0_client" "storefront" {
  name                = "Storefront"
  app_type            = "spa"
  callbacks           = ["https://${aws_cloudfront_distribution.storefront.domain_name}/callback"]
  allowed_web_origins = ["https://${aws_cloudfront_distribution.storefront.domain_name}"]
}

resource "auth0_client" "worker" {
  name     = "Order worker"
  app_type = "non_interactive"
}

resource "auth0_client_grant" "worker_api" {
  client_id = auth0_client.worker.id
  audience  = auth0_resource_server.api.identifier
  scopes    = ["read:orders"]
}

resource "auth0_connection" "users" {
  name     = "Username-Password-Authentication"
  strategy = "auth0"
}

resource "auth0_connection_clients" "users" {
  connection_id   = auth0_connection.users.id
  enabled_clients = [auth0_client.storefront.id]
}

resource "auth0_action" "add_customer_id" {
  name   = "add-customer-id"
  code   = "exports.onExecutePostLogin = async (event, api) => {};"
  deploy = true
  supported_triggers {
    id      = "post-login"
    version = "v3"
  }
}

resource "auth0_log_stream" "events" {
  name = "events"
  type = "eventbridge"
  sink {
    aws_account_id = "123456789012"
    aws_region     = "eu-central-1"
  }
}
