resource "auth0_client" "web" {
  name     = "Web"
  app_type = "spa"
}

resource "auth0_client" "worker" {
  name     = "Worker"
  app_type = "non_interactive"
}

resource "auth0_resource_server" "api" {
  name       = "Orders API"
  identifier = "https://api.example.com"
}

resource "auth0_resource_server_scopes" "api" {
  resource_server_identifier = auth0_resource_server.api.identifier
  scopes {
    name = "read:orders"
  }
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
  enabled_clients = [auth0_client.web.id]
}

resource "auth0_action" "enrich" {
  name   = "enrich-token"
  code   = "exports.onExecutePostLogin = async () => {};"
  deploy = true
  supported_triggers {
    id      = "post-login"
    version = "v3"
  }
}

resource "auth0_trigger_action" "enrich" {
  trigger   = "post-login"
  action_id = auth0_action.enrich.id
}

resource "auth0_log_stream" "events" {
  name = "events"
  type = "eventbridge"
  sink {
    aws_account_id = "123456789012"
    aws_region     = "eu-central-1"
  }
}

resource "auth0_role" "admin" {
  name = "admin"
}

resource "auth0_role_permissions" "admin" {
  role_id = auth0_role.admin.id
  permissions {
    name                       = "read:orders"
    resource_server_identifier = auth0_resource_server.api.identifier
  }
}
