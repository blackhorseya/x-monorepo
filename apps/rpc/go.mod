module github.com/blackhorseya/x-monorepo/apps/rpc

go 1.27

require (
	connectrpc.com/connect v1.21.1
	github.com/blackhorseya/x-monorepo/pkgs/ordering v0.0.0
	github.com/blackhorseya/x-monorepo/pkgs/platform v0.0.0
	github.com/google/wire v0.7.0
)

require (
	github.com/blackhorseya/x-monorepo/pkgs/kernel v0.0.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

replace (
	github.com/blackhorseya/x-monorepo/pkgs/kernel => ../../pkgs/kernel
	github.com/blackhorseya/x-monorepo/pkgs/ordering => ../../pkgs/ordering
	github.com/blackhorseya/x-monorepo/pkgs/platform => ../../pkgs/platform
)
