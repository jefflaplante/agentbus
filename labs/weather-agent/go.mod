module weather

go 1.26.2

require github.com/jefflaplante/agentbus v0.0.0-00010101000000-000000000000

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
)

// The labs build against this checkout. Outside the repository, remove
// this line and `go mod tidy` fetches the published module.
replace github.com/jefflaplante/agentbus => ../..
