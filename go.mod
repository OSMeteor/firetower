module github.com/OSMeteor/firetower

go 1.12

require (
github.com/golang/protobuf v1.5.4
github.com/gorilla/websocket v1.5.1
github.com/holdno/snowFlakeByGo v1.0.0
github.com/json-iterator/go v1.1.12
github.com/pelletier/go-toml v1.9.5
github.com/pkg/errors v0.9.1
golang.org/x/net v0.25.0
google.golang.org/grpc v1.64.0
)

replace google.golang.org/grpc => github.com/grpc/grpc-go v1.64.0
replace golang.org/x/net => github.com/golang/net v0.25.0
