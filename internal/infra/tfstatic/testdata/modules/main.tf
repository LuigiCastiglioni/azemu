variable "enabled" {
  default = true
}

module "solo" {
  count  = var.enabled ? 1 : 0
  source = "./child"

  name = "solo"
  tier = "premium"
}

module "each" {
  for_each = toset(["a", "b"])
  source   = "./child"

  name = each.key
  tier = module.solo[0].account
}

module "off" {
  count  = var.enabled ? 0 : 1
  source = "./child"
  name   = "off"
}

resource "azurerm_key_vault" "kv" {
  name                = "kv-${join("-", [for m in module.each : m.account])}"
  resource_group_name = "rg"
  location            = "northeurope"
  tenant_id           = module.each["a"].id

  tags = {
    splat = join(",", module.solo[*].account)  # module.solo[*].attr
    dyn   = module.each[local.key].account     # module.each[expr].attr
  }
}

locals {
  key = "b"
}
