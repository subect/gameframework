package client

import "time"

// ClientConfig describes how to connect a game client to a gameframework server.
// This is intentionally minimal and focused on network concerns only; game logic
// / rendering should sit on top of this package.
type ClientConfig struct {
	// ServerAddr is the UDP address of the game server, e.g. "127.0.0.1:30000".
	ServerAddr string

	// PlayerID is the logical player identifier assigned by your game.
	PlayerID uint16

	// TickHz is the expected simulation tick rate. This should normally match
	// the server's tick rate.
	TickHz int

	// Optional dial / resend settings. Zero values mean "use defaults".
	DialTimeout   time.Duration
	ResendTimeout time.Duration
}

// FrameUpdate represents a single server frame as observed by the client.
// It is the main data structure that game / rendering layers should consume.
type FrameUpdate struct {
	Tick   uint32
	Inputs map[uint16]uint32
	Snap   []byte // optional snapshot payload
}

// Client is the main entry point for connecting to a gameframework server
// from a game. It hides UDP / reliable channel details behind a small API.
//
// NOTE: The implementation here is intentionally minimal and is meant to be
// filled in based on concrete usage (e.g. the ballbattle demo). The API shape
// should remain relatively stable for real projects.
type Client struct {
	cfg     ClientConfig
	updates chan FrameUpdate
}

// NewClient creates a new Client instance with the given configuration.
// It does not start any background goroutines; call Start to begin network IO.
func NewClient(cfg ClientConfig) (*Client, error) {
	c := &Client{
		cfg:     cfg,
		updates: make(chan FrameUpdate, 128),
	}
	return c, nil
}

// Start begins the client's network processing. In a real implementation
// this would create the UDP socket, start read / write loops, handle ping/pong,
// reliable messages, etc.
func (c *Client) Start() error {
	// TODO: implement network connection / loops based on concrete usage.
	return nil
}

// Close shuts down the client and releases any underlying resources.
func (c *Client) Close() error {
	// TODO: implement graceful shutdown and socket close.
	return nil
}

// SendInput sends a single input for the given simulation tick to the server.
// The concrete wire format should mirror proto.InputPacket.
func (c *Client) SendInput(tick uint32, input uint32) error {
	// TODO: implement input packet send.
	return nil
}

// FrameUpdates returns a receive-only channel of FrameUpdate. The client
// implementation should push new frames here when they arrive from the server.
func (c *Client) FrameUpdates() <-chan FrameUpdate {
	return c.updates
}

// RTT returns the most recent round-trip time estimation between client and server.
func (c *Client) RTT() time.Duration {
	// TODO: compute and track RTT via ping/pong.
	return 0
}

// LossRate returns an approximate recent packet loss rate as a fraction [0,1].
func (c *Client) LossRate() float64 {
	// TODO: track packet loss statistics.
	return 0
}
