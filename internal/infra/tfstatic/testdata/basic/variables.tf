variable "env" {
  type    = string
  default = "dev"
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "apps" {
  type = map(object({
    runtime  = optional(string, "python")
    docker   = optional(bool, false)
    image    = optional(string, "")
    port     = optional(number)
    settings = optional(map(string), {})
  }))
}
