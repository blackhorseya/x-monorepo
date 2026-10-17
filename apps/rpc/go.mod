module github.com/blackhorseya/x-monorepo/apps/rpc

go 1.27

require (
	connectrpc.com/connect v1.21.1
	github.com/blackhorseya/x-monorepo/pkgs/ordering v0.0.0
	github.com/blackhorseya/x-monorepo/pkgs/platform v0.0.0
	github.com/google/wire v0.7.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/blackhorseya/x-monorepo/pkgs/kernel v0.0.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.25.0 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/blackhorseya/x-monorepo/pkgs/kernel => ../../pkgs/kernel
	github.com/blackhorseya/x-monorepo/pkgs/ordering => ../../pkgs/ordering
	github.com/blackhorseya/x-monorepo/pkgs/platform => ../../pkgs/platform
)
