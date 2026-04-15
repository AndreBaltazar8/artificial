// Module: a deliberately trivial plugin used to exercise the pluginhost
// loader without pulling the game-krunker-harness repo into this
// workspace. Lives under scratch/ because it is not part of the
// production artificial build.
//
// The local replace on pkg-go-shared lets this binary be built without
// a tagged release of the shared module — the same pattern the real
// harness plugin uses on its own disk today.
module scratch/stub-plugin

go 1.25.1

replace artificial.pt/pkg-go-shared => ../../src/pkg-go-shared

require artificial.pt/pkg-go-shared v0.0.0-00010101000000-000000000000

require (
	github.com/fatih/color v1.13.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/hashicorp/go-hclog v1.6.3 // indirect
	github.com/hashicorp/go-plugin v1.7.0 // indirect
	github.com/hashicorp/yamux v0.1.2 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.17 // indirect
	github.com/oklog/run v1.1.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20231106174013-bbf56f31fb17 // indirect
	google.golang.org/grpc v1.61.0 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
)
