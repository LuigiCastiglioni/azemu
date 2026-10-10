variable "obj" {
  default = { a = 1 }
}

resource "azurerm_resource_group" "rg" {
  name     = "rg"
  location = "northeurope"
  tags = {
    cidr     = cidrsubnet("10.0.0.0/16", 8, 2)
    fmt      = format("%s-%03d", "x", 7)
    lookup   = lookup({ a = "1" }, "b", "dflt")
    try      = try(var.obj.nope, "fallback")
    can      = tostring(can(regex("^b", "ba")))
    file     = file("${path.module}/data.txt")
    tpl      = templatefile("${path.module}/tpl.txt", { who = "me" })
    sha      = sha256("a")
    b64      = base64encode("hi")
    json     = jsonencode({ a = [1, 2] })
    startswith = tostring(startswith("hello", "he"))
    replace  = replace("a.b.c", ".", "-")
    ws       = terraform.workspace
    coalesce = coalesce("", "z")
    one      = one([]) == null ? "null" : "set"
    nofn     = no_such_fn(1)
    prov     = provider::foo::bar("x")
  }
}
