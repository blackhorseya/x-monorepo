module github.com/blackhorseya/x-monorepo/pkgs/ordering

go 1.27

require (
	connectrpc.com/connect v1.21.1
	github.com/blackhorseya/x-monorepo/pkgs/kernel v0.0.0
	github.com/blackhorseya/x-monorepo/pkgs/platform v0.0.0
	github.com/google/wire v0.7.0
	google.golang.org/protobuf v1.36.12
)

replace (
	github.com/blackhorseya/x-monorepo/pkgs/kernel => ../kernel
	github.com/blackhorseya/x-monorepo/pkgs/platform => ../platform
)
