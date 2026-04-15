package plugin

import (
	"encoding/gob"
	"net/rpc"
)

// gob only knows how to encode types it has seen. json.RawMessage is an
// alias for []byte so it round-trips fine, but ToolDescriptor carries it
// inside a struct — register the concrete slice type once at package init
// so rpc.Server/Client don't panic on first use.
func init() {
	gob.Register([]ToolDescriptor{})
}

// ExecuteArgs is the net/rpc wire-form of an Execute call.
//
// Kept to primitive types (string + []byte) so gob encoding is trivial
// and plugin authors never hit a "gob: type not registered" panic on
// tool input shapes.
type ExecuteArgs struct {
	ToolName string
	ArgsJSON []byte
}

// ExecuteReply is the net/rpc wire-form of an Execute result.
//
// Err is a plain string because gob cannot encode the error interface
// directly; the client-side shim re-wraps it into a Go error before
// returning to the caller.
type ExecuteReply struct {
	ResultJSON []byte
	Err        string
}

// rpcClient is the worker-side adapter that satisfies the ArtificialPlugin
// interface by delegating to a net/rpc.Client held by go-plugin.
//
// It is what the pluginhost loader receives when it dispenses a loaded
// plugin; the loader treats it as an ordinary ArtificialPlugin and never
// needs to know an RPC boundary is in play.
type rpcClient struct{ client *rpc.Client }

func (c *rpcClient) Name() string {
	var reply string
	_ = c.client.Call("Plugin.Name", Empty{}, &reply)
	return reply
}

func (c *rpcClient) Tools() []ToolDescriptor {
	var reply []ToolDescriptor
	_ = c.client.Call("Plugin.Tools", Empty{}, &reply)
	return reply
}

func (c *rpcClient) Execute(toolName string, argsJSON []byte) ([]byte, error) {
	var reply ExecuteReply
	if err := c.client.Call("Plugin.Execute", ExecuteArgs{ToolName: toolName, ArgsJSON: argsJSON}, &reply); err != nil {
		return nil, err
	}
	if reply.Err != "" {
		return reply.ResultJSON, rpcError(reply.Err)
	}
	return reply.ResultJSON, nil
}

// rpcServer is the plugin-side net/rpc handler that wraps a concrete
// ArtificialPlugin implementation. net/rpc reaches these methods via
// reflection, so the signatures (value receiver on exported methods,
// two args, error return) are dictated by the rpc package contract.
type rpcServer struct{ Impl ArtificialPlugin }

func (s *rpcServer) Name(_ Empty, reply *string) error {
	*reply = s.Impl.Name()
	return nil
}

func (s *rpcServer) Tools(_ Empty, reply *[]ToolDescriptor) error {
	*reply = s.Impl.Tools()
	return nil
}

func (s *rpcServer) Execute(args ExecuteArgs, reply *ExecuteReply) error {
	out, err := s.Impl.Execute(args.ToolName, args.ArgsJSON)
	reply.ResultJSON = out
	if err != nil {
		reply.Err = err.Error()
	}
	return nil
}

// Empty is a zero-arg placeholder for net/rpc methods that take no
// meaningful input. It has to be exported (rather than a lowercase
// `empty` type) because net/rpc's reflection-based method scanner skips
// any method whose argument types aren't exported or builtin — an
// unexported type silently drops the method from the service.
type Empty struct{}

// rpcError round-trips an error-string from the plugin back to the worker.
// Callers should not rely on error identity — match on string if needed.
type rpcError string

func (e rpcError) Error() string { return string(e) }
