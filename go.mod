module github.com/gke-demos/ax-bug-bash

go 1.27.1

replace github.com/google/ax => /usr/local/google/home/mofi/Documents/github.com/google/ax

require (
	github.com/google/ax v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/agent-substrate/env v0.0.11-0.20260912052224-4468a200b170 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
)
