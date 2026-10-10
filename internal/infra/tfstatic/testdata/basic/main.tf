locals {
  common = merge(var.tags, { env = var.env })
}

resource "azurerm_storage_account" "st" {
  name                = "${local.prefix}st"
  resource_group_name = "rg"
  location            = "northeurope"
}

resource "azurerm_linux_function_app" "app" {
  for_each = var.apps

  name                 = "${local.prefix}-${each.key}"
  storage_account_name = azurerm_storage_account.st.name
  app_settings = merge(
    each.value.settings,
    { TABLE = azurerm_storage_account.st.primary_table_endpoint },
    each.key == "b" ? { EXTRA = "1" } : {},
  )

  site_config {
    dynamic "application_stack" {
      for_each = each.value.docker ? [] : [1]
      content {
        python_version = each.value.runtime
      }
    }
    dynamic "application_stack" {
      for_each = each.value.docker ? [1] : []
      content {
        docker {
          image_name = each.value.image
        }
      }
    }
  }

  tags = local.common

  lifecycle {
    ignore_changes = [tags]
  }
}

# not an azurerm_ resource: evaluated only when referenced, never emitted
resource "terraform_data" "unused" {
  input = var.does_not_exist
}
