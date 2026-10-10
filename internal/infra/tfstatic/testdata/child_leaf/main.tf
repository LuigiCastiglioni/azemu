variable "name" {}

resource "azurerm_storage_queue" "q" {
  name                 = "q-${var.name}"
  storage_account_name = "x"
}
