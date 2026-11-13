resource "azurerm_resource_group" "example" {
  name     = "rg-aks-acr"
  location = "westeurope"
}

resource "azurerm_container_registry" "example" {
  name                = "acraksexample"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  sku                 = "Standard"
}

# One identity pulls images for both the cluster's nodes and the web app.
resource "azurerm_user_assigned_identity" "pull" {
  name                = "id-acr-pull"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
}

resource "azurerm_role_assignment" "pull" {
  principal_id         = azurerm_user_assigned_identity.pull.principal_id
  scope                = azurerm_container_registry.example.id
  role_definition_name = "AcrPull"
}

resource "azurerm_kubernetes_cluster" "example" {
  name                = "aks-example"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  dns_prefix          = "aks-example"

  default_node_pool {
    name       = "system"
    node_count = 1
    vm_size    = "Standard_D2s_v5"
  }

  identity {
    type = "SystemAssigned"
  }

  kubelet_identity {
    client_id                 = azurerm_user_assigned_identity.pull.client_id
    object_id                 = azurerm_user_assigned_identity.pull.principal_id
    user_assigned_identity_id = azurerm_user_assigned_identity.pull.id
  }
}

resource "azurerm_service_plan" "example" {
  name                = "asp-example"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  os_type             = "Linux"
  sku_name            = "B1"
}

resource "azurerm_linux_web_app" "example" {
  name                = "app-aks-acr-example"
  resource_group_name = azurerm_resource_group.example.name
  location            = azurerm_resource_group.example.location
  service_plan_id     = azurerm_service_plan.example.id

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.pull.id]
  }

  site_config {
    container_registry_use_managed_identity       = true
    container_registry_managed_identity_client_id = azurerm_user_assigned_identity.pull.client_id

    application_stack {
      docker_image_name   = "web:latest"
      docker_registry_url = "https://${azurerm_container_registry.example.login_server}"
    }
  }
}
