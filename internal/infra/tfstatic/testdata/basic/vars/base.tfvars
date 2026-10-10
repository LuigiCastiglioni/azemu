env  = "test"
tags = { owner = "me" }
apps = {
  a = { runtime = "3.12" }
  b = {
    docker   = true
    image    = "img"
    settings = { K = "v" }
  }
}
