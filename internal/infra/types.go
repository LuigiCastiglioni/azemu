package infra

// Model is the distilled, cached view of `terraform plan` that the rest of azemu consumes.
// Stored at <StateDir>/infra/<env>.json with mode 0600 (it contains secrets).
type Model struct {
	Env             string           `json:"env"`
	Apps            []App            `json:"apps"`
	StorageAccounts []StorageAccount `json:"storage_accounts"`
	Vaults          []string         `json:"vaults"` // key vault names
	Warnings        []string         `json:"warnings,omitempty"`
}

type App struct {
	Name            string            `json:"name"`                      // Azure resource name, e.g. eund-func-orders
	ResourceType    string            `json:"resource_type"`             // e.g. azurerm_function_app_flex_consumption
	Host            string            `json:"host"`                      // "functions" | "appservice"
	Kind            string            `json:"kind"`                      // "zip" | "docker"
	Runtime         string            `json:"runtime,omitempty"`         // python | node | dotnet-isolated | java
	RuntimeVersion  string            `json:"runtime_version,omitempty"` // "3.12"
	DockerImage     string            `json:"docker_image,omitempty"`    // repo:tag without registry
	AppSettings     map[string]string `json:"app_settings"`
	UnknownSettings []string          `json:"unknown_settings,omitempty"` // keys known only after apply
	EasyAuth        bool              `json:"easy_auth"`
	HostStorage     string            `json:"host_storage,omitempty"`    // functions: host storage account name
	HostStorageMI   bool              `json:"host_storage_mi,omitempty"` // functions: identity-based AzureWebJobsStorage
	CommandLine     string            `json:"command_line,omitempty"`    // app_command_line
}

type StorageAccount struct {
	Name       string   `json:"name"`
	Containers []string `json:"containers,omitempty"`
	Tables     []string `json:"tables,omitempty"`
	Queues     []string `json:"queues,omitempty"`
}

// App returns the app with the given resource name, or nil.
func (m *Model) App(name string) *App {
	for i := range m.Apps {
		if m.Apps[i].Name == name {
			return &m.Apps[i]
		}
	}
	return nil
}
