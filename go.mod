module github.com/OSMeteor/firetower

go 1.12

require (
        github.com/golang/protobuf v0.0.0
        github.com/gorilla/websocket v0.0.0
        github.com/holdno/snowFlakeByGo v0.0.0
        github.com/json-iterator/go v0.0.0
        github.com/pelletier/go-toml v0.0.0
        github.com/pkg/errors v0.0.0
        golang.org/x/net v0.0.0
        google.golang.org/grpc v0.0.0
)

replace github.com/json-iterator/go => ./internal/stub/jsoniter
replace github.com/gorilla/websocket => ./internal/stub/websocket
replace github.com/holdno/snowFlakeByGo => ./internal/stub/snowflake
replace github.com/pelletier/go-toml => ./internal/stub/toml
replace github.com/golang/protobuf => ./internal/stub/proto
replace golang.org/x/net => ./internal/stub/xnet
replace google.golang.org/grpc => ./internal/stub/grpc
replace github.com/pkg/errors => ./internal/stub/errors
