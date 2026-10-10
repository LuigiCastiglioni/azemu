variable "name" {
  type = string
}

variable "tier" {
  type    = string
  default = "std"
}

resource "azurerm_storage_account" "this" {
  name                = "st${var.name}"
  resource_group_name = "rg"
  location            = "northeurope"
  account_tier        = var.tier
}

module "grand" {
  source = "../../child_leaf"
  name   = var.name
}

output "account" {
  value = azurerm_storage_account.this.name
}

output "id" {
  value = azurerm_storage_account.this.id
}
