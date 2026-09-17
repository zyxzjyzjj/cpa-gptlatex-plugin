module github.com/zyxzjyzjj/cpa-gptlatex-plugin

go 1.26.0

// Intentionally no dependency on github.com/router-for-me/CLIProxyAPI/v7.
// The host wires the plugin in over a C ABI plus JSON, so nothing here needs
// CPA's Go packages — and importing them was actively harmful: the module you
// build against decides the schema_version the plugin advertises, and the host
// refuses any plugin whose schema_version exceeds its own
// (internal/pluginhost/rpc_client.go). Pinning v7.2.129 against a v7.2.50 host
// advertised 3 where only 1 is accepted, so the plugin would not load at all.
// The handful of ABI constants needed are mirrored in main.go instead.

require gopkg.in/yaml.v3 v3.0.1

require github.com/gorilla/websocket v1.5.3
