# referenced from main.tf before it is declared; files are order-independent
locals {
  short  = "eu"
  prefix = "${local.short}${var.env}"
}
