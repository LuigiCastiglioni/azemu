locals {
  a = local.b
  b = local.a
}

resource "azurerm_resource_group" "rg" {
  name     = "rg"
  location = local.a
}
