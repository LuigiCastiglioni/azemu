resource "azurerm_storage_account" "st" {
  name                = "st"
  resource_group_name = "rg"
  location            = "northeurope"
}

# for_each over something Azure assigns: no instances can be derived, warn and skip
resource "azurerm_storage_container" "each" {
  for_each             = toset(split(",", azurerm_storage_account.st.primary_access_key))
  name                 = each.key
  storage_account_name = azurerm_storage_account.st.name
}

# count depends on an unknown: one instance is assumed
resource "azurerm_storage_queue" "counted" {
  count                = azurerm_storage_account.st.id == "x" ? 0 : 2
  name                 = "q${count.index}"
  storage_account_name = azurerm_storage_account.st.name
}

# whole resource referenced in a for expression: attributes are discovered from the error
locals {
  endpoints = [for a in azurerm_storage_account.st[*] : a.primary_blob_endpoint]
}

resource "azurerm_key_vault" "kv" {
  name                = "kv"
  resource_group_name = "rg"
  location            = "northeurope"
  tenant_id           = join(",", local.endpoints)
  tags = {
    # unknown string inside a conditional object: the key must survive
    maybe = true ? "x" : azurerm_storage_account.st.id
  }
}

module "self" {
  source = "./"
}
