module github.com/blackhorseya/x-monorepo/pkgs/payment

go 1.27

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.2
	connectrpc.com/connect v1.21.1
	github.com/blackhorseya/x-monorepo/pkgs/platform v0.0.0
	google.golang.org/protobuf v1.36.12
)

require github.com/blackhorseya/x-monorepo/pkgs/kernel v0.0.0 // indirect

replace (
	github.com/blackhorseya/x-monorepo/pkgs/kernel => ../kernel
	github.com/blackhorseya/x-monorepo/pkgs/platform => ../platform
)
