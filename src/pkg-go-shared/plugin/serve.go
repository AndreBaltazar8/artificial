package plugin

import (
	"net/rpc"

	goplugin "github.com/hashicorp/go-plugin"
)

// Handshake is the magic handshake that guards against accidental plugin
// loads of unrelated binaries. Workers and plugin binaries MUST agree on
// all three fields — if they don't, go-plugin fails the load loudly with
// a protocol mismatch error instead of silently returning gibberish.
//
// ProtocolVersion must be bumped whenever the wire-form of this package
// changes in a non-backward-compatible way. Additive changes (new
// optional fields on ToolDescriptor, new optional RPC methods) do not
// require a bump.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "ARTIFICIAL_PLUGIN",
	MagicCookieValue: "14-a5b7c9d-plugin-handshake",
}

// PluginMap is the registry of plugin interface implementations exposed
// over go-plugin. v1 has a single named interface ("artificial") because
// every plugin implements the same ArtificialPlugin contract — plugins do
// NOT differentiate themselves via this map, they differentiate via their
// Name() return value.
//
// The map is keyed by a plugin-type identifier that both sides negotiate
// on; here we use "artificial" since the entire fleet speaks one contract.
var PluginMap = map[string]goplugin.Plugin{
	"artificial": &ArtificialGoPlugin{},
}

// ArtificialGoPlugin is the go-plugin adapter that bridges the
// ArtificialPlugin Go interface into the netrpc transport go-plugin uses
// under the hood.
//
// Plugin-side authors construct this struct with their concrete Impl set
// and hand it to goplugin.Serve; worker-side callers leave Impl nil and
// receive a client-shim via Client() after Dispense.
//
// We deliberately use netrpc rather than gRPC here: the payloads are
// simple (strings and []byte), the throughput is low (human-driven tool
// calls), and netrpc avoids pulling in protoc + generated code for every
// plugin author. Swapping to gRPC later is a drop-in change under a new
// ProtocolVersion — consumers of this package don't need to know.
type ArtificialGoPlugin struct {
	Impl ArtificialPlugin
}

// Server is called on the plugin side to produce the net/rpc receiver
// that go-plugin will register and serve on the stdio channel.
func (p *ArtificialGoPlugin) Server(*goplugin.MuxBroker) (any, error) {
	return &rpcServer{Impl: p.Impl}, nil
}

// Client is called on the worker side after go-plugin has dialed the
// plugin subprocess. It wraps the rpc.Client into something that
// satisfies the ArtificialPlugin interface so the loader can treat every
// loaded plugin uniformly.
func (*ArtificialGoPlugin) Client(_ *goplugin.MuxBroker, c *rpc.Client) (any, error) {
	return &rpcClient{client: c}, nil
}

// Serve is the entry point plugin binaries call from their main() to
// hand control over to go-plugin. It blocks until the worker that
// launched the plugin disconnects or the process is killed.
//
// Typical plugin main():
//
//	func main() {
//	    plugin.Serve(&myPlugin{})
//	}
//
// where *myPlugin satisfies ArtificialPlugin. Serve handles the magic
// handshake, the netrpc server setup, and the stdio plumbing — plugin
// authors only ever write the ArtificialPlugin methods.
func Serve(impl ArtificialPlugin) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: map[string]goplugin.Plugin{
			"artificial": &ArtificialGoPlugin{Impl: impl},
		},
	})
}
