module github.com/beeos-ai/bma-executor-sdk

go 1.25.0

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/anthropics/anthropic-sdk-go/v2 v2.7.0
	github.com/beeos-ai/bma-sdk/v3 v3.1.0
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/hanwen/go-fuse/v2 v2.12.1
	github.com/itchyny/gojq v0.12.20
	github.com/spf13/cobra v1.10.2
	golang.org/x/sys v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.6 // indirect
	github.com/go-logr/logr v1.4.2 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/itchyny/timefmt-go v0.1.9 // indirect
	github.com/oapi-codegen/runtime v1.8.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	github.com/standard-webhooks/standard-webhooks/libraries v0.0.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.33.0 // indirect
	go.opentelemetry.io/otel/metric v1.33.0 // indirect
	go.opentelemetry.io/otel/trace v1.33.0 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/sync v0.16.0 // indirect
)

retract [v0.0.0, v1.12.1] // superseded by v1.12.2 (same API)
