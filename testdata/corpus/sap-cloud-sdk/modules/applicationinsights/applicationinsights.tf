terraform {
  required_providers {
    azurerm = {
      version = "~>3.47.0"
      source  = "hashicorp/azurerm"
    }
    azurecaf = {
      source  = "aztfmod/azurecaf"
      version = "~>1.2.15"
    }
  }
}
# ------------------------------------------------------------------------------------------------------
# DEPLOY APPLICATION INSIGHTS
# ------------------------------------------------------------------------------------------------------
resource "azurecaf_name" "applicationinsights_name" {
  name          = var.resource_token
  resource_type = "azurerm_application_insights"
  random_length = 0
  clean_input   = true
}

resource "azurerm_application_insights" "applicationinsights" {
  name                = azurecaf_name.applicationinsights_name.result
  location            = var.location
  resource_group_name = var.rg_name
  application_type    = "web"
  workspace_id        = var.workspace_id
  tags                = var.tags
}

data "azurerm_subscription" "current" {}

resource "azurerm_portal_dashboard" "board" {
  name                 = "dash-${var.resource_token}"
  resource_group_name  = var.rg_name
  location             = var.location
  tags                 = var.tags
  # azemu corpus: the original heredoc (~65KB of dashboard JSON) is truncated to this equivalent
  # shape; it keeps the ${...} interpolations of the original.
  dashboard_properties = <<DASH
{
    "lenses": {
        "0": {
            "order": 0,
            "parts": {
                "0": {
                    "position": { "x": 0, "y": 0, "colSpan": 2, "rowSpan": 1 },
                    "metadata": {
                        "inputs": [
                            {
                                "name": "id",
                                "value": "/subscriptions/${data.azurerm_subscription.current.subscription_id}/resourceGroups/${var.rg_name}/providers/Microsoft.Insights/components/${azurerm_application_insights.applicationinsights.name}"
                            },
                            {
                                "name": "Version",
                                "value": "1.0"
                            }
                        ],
                        "type": "Extension/AppInsightsExtension/PartType/AppMapGalleryPinnedPart",
                        "asset": {
                            "idInputName": "id",
                            "type": "ApplicationInsights"
                        }
                    }
                }
            }
        }
    },
    "metadata": {
        "model": {}
    }
}
DASH
}
