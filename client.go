package sc

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scgolang/osc"
)

// OSC addresses.
// See http://doc.sccode.org/Reference/Server-Command-Reference.html.
const (
	statusAddress              = "/status"
	statusReplyAddress         = "/status.reply"
	synthdefReceiveAddress     = "/d_recv"
	dumpOscAddress             = "/dumpOSC"
	doneOscAddress             = "/done"
	synthNewAddress            = "/s_new"
	groupNewAddress            = "/g_new"
	groupHeadAddress           = "/g_head"
	groupTailAddress           = "/g_tail"
	groupFreeAllAddress        = "/g_freeAll"
	groupDeepFreeAddress       = "/g_deepFree"
	groupDumpTreeAddress       = "/g_dumpTree"
	groupQueryTreeAddress      = "/g_queryTree"
	groupQueryTreeReplyAddress = "/g_queryTree.reply"
	bufferAllocAddress         = "/b_alloc"
	bufferReadAddress          = "/b_allocRead"
	bufferReadChannelAddress   = "/b_allocReadChannel"
	bufferGenAddress           = "/b_gen"
	bufferInfoAddress          = "/b_info"
	bufferQueryAddress         = "/b_query"
)

// Arguments to dumpOSC command.
// See http://doc.sccode.org/Reference/Server-Command-Reference.html#/dumpOSC
const (
	DumpOff      = 0
	DumpParsed   = 1
	DumpContents = 2
	DumpAll      = 3
)

// Arguments to s_new command.
// See http://doc.sccode.org/Reference/Server-Command-Reference.html#/s_new
const (
	AddToHead  = int32(0)
	AddToTail  = int32(1)
	AddBefore  = int32(2)
	AddAfter   = int32(3)
	AddReplace = int32(4)
)

const (
	// RootNodeID is what sclang uses as the root node ID. See http://doc.sccode.org/Classes/RootNode.html.
	RootNodeID = int32(0)

	// DefaultGroupID is what sclang uses for the default group ID. See http://doc.sccode.org/Reference/default_group.html.
	DefaultGroupID = int32(1)

	// DefaultLocalAddr is the listening address for DefaultClient.
	DefaultLocalAddr = "0.0.0.0:57110"

	// DefaultScsynthAddr is the remote address for DefaultClient.
	DefaultScsynthAddr = "0.0.0.0:57120"

	// DefaultConnectTimeout is the default timeout for connecting to scsynth.
	DefaultConnectTimeout = time.Second
)

// Common errors.
var (
	ErrTimeout = errors.New("timeout error")
)

// Client manages all communication with scsynth
type Client struct {
	// errChan is a channel that emits errors from
	// the goroutine that runs the OSC server that is
	// used to receive messages from scsynth
	errChan    chan error
	closeMutex sync.Mutex
	closed     int32

	addr    *net.UDPAddr
	oscConn osc.Conn

	bufferInfoChan chan osc.Message // bufferInfoChan relays /b_info messages
	doneChan       chan osc.Message // doneChan relays /done messages
	statusChan     chan osc.Message // statusChan relays /status.reply messages
	gqueryTreeChan chan osc.Message // gqueryTreeChan relays /done messages

	nextSynthID int32 // next synth node ID
}

// number of concurrent handlers for /done messages.
const numDoneHandlers = 8

// NewClient creates a new SuperCollider client.
// The client will bind to the provided address and port
// to receive messages from scsynth.
func NewClient(network, local, scsynth string, timeout time.Duration) (*Client, error) {
	addr, err := net.ResolveUDPAddr(network, local)
	if err != nil {
		return nil, err
	}
	c := &Client{
		errChan:        make(chan error),
		bufferInfoChan: make(chan osc.Message),
		doneChan:       make(chan osc.Message, numDoneHandlers),
		gqueryTreeChan: make(chan osc.Message),
		statusChan:     make(chan osc.Message),
		addr:           addr,
		nextSynthID:    1000,
	}
	if err := c.Connect(scsynth, timeout); err != nil {
		return nil, err
	}
	return c, nil
}

var (
	defaultClient *Client
	defaultGroup  *GroupNode
)

// DefaultClient returns the default sc client.
func DefaultClient() (*Client, error) {
	var err error

	if defaultClient == nil {
		defaultClient, err = NewClient("udp", DefaultLocalAddr, DefaultScsynthAddr, DefaultConnectTimeout)
		if err != nil {
			return nil, err
		}
		defaultGroup, err = defaultClient.AddDefaultGroup()
		if err != nil {
			return nil, err
		}
	}
	return defaultClient, nil
}

// Connect connects to an scsynth instance via UDP.
func (c *Client) Connect(addr string, timeout time.Duration) error {
	raddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return err
	}

	// Attempt connection with a timeout.
	var (
		start    = time.Now()
		timedOut = true
	)
	for time.Now().Sub(start) < timeout {
		oscConn, err := osc.DialUDP("udp", c.addr, raddr)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		c.oscConn = oscConn
		timedOut = false
		break
	}
	if timedOut {
		return errors.New("connection timeout")
	}

	// listen for OSC messages
	go func(errChan chan error) {
		var (
			start = time.Now()
			err   error
		)
		for time.Now().Sub(start) < timeout {
			err = c.oscConn.Serve(8, c.oscHandlers()) // Arbitrary number of worker routines.
			if err != nil {
				time.Sleep(100 * time.Second)
				continue
			}
		}
		if err != nil {
			errChan <- err
		}
	}(c.errChan)

	return nil
}

// Status gets the status of scsynth with a timeout.
// If the status request times out it returns ErrTimeout.
func (c *Client) Status(timeout time.Duration) (*ServerStatus, error) {
	statusReq := osc.Message{
		Address: statusAddress,
	}
	if err := c.oscConn.Send(statusReq); err != nil {
		return nil, err
	}

	after := time.After(timeout)

	select {
	case _ = <-after:
		return nil, ErrTimeout
	case msg := <-c.statusChan:
		return newStatus(msg)
	case err := <-c.errChan:
		return nil, err
	}
}

// SendAllDefs sends all the synthdefs that have been registered with RegisterSynthdef.
func (c *Client) SendAllDefs() error {
	synthdefsMu.RLock()
	defer synthdefsMu.RUnlock()
	for _, def := range Synthdefs {
		if err := c.SendDef(def); err != nil {
			return err
		}
	}
	return nil
}

// SendDef sends a synthdef to scsynth.
// This method blocks until a /done message is received
// indicating that the synthdef was loaded
func (c *Client) SendDef(def *Synthdef) error {
	db, err := def.Bytes()
	if err != nil {
		return err
	}
	msg := osc.Message{
		Address: synthdefReceiveAddress,
		Arguments: osc.Arguments{
			osc.Blob(db),
		},
	}
	if err := c.oscConn.Send(msg); err != nil {
		return err
	}
	var done osc.Message
	select {
	case done = <-c.doneChan:
		goto ParseMessage
	case err = <-c.errChan:
		return err
	}

ParseMessage:
	// error if this message was not an ack of the synthdef
	errmsg := "expected /done with /d_recv argument"
	if len(done.Arguments) != 1 {
		return fmt.Errorf(errmsg)
	}
	addr, err := done.Arguments[0].ReadString()
	if err != nil {
		return err
	}
	if addr != synthdefReceiveAddress {
		return errors.New(errmsg)
	}
	return nil
}

// DumpOSC sends a /dumpOSC message to scsynth
// level should be DumpOff, DumpParsed, DumpContents, DumpAll
func (c *Client) DumpOSC(level int32) error {
	return c.oscConn.Send(osc.Message{
		Address: dumpOscAddress,
		Arguments: osc.Arguments{
			osc.Int(level),
		},
	})
}

// Synth creates a synth node.
func (c *Client) Synth(defName string, id, action, target int32, ctls map[string]float32) (*Synth, error) {
	msg := osc.Message{
		Address: synthNewAddress,
		Arguments: osc.Arguments{
			osc.String(defName),
			osc.Int(id),
			osc.Int(action),
			osc.Int(target),
		},
	}
	if ctls != nil {
		for k, v := range ctls {
			msg.Arguments = append(msg.Arguments, osc.String(k))
			msg.Arguments = append(msg.Arguments, osc.Float(v))
		}
	}
	if err := c.oscConn.Send(msg); err != nil {
		return nil, err
	}
	return newSynth(c, defName, id), nil
}

// SynthArgs contains the arguments necessary to create a synth that is part of a group.
type SynthArgs struct {
	DefName string
	ID      int32
	Action  int32
	Target  int32
	Ctls    map[string]float32
}

// Synths creates multiple synth nodes at once with an OSC bundle.
func (c *Client) Synths(args []SynthArgs) error {
	bun := osc.Bundle{
		Packets: make([]osc.Packet, len(args)),
	}
	for i, arg := range args {
		msg := osc.Message{
			Address: synthNewAddress,
			Arguments: osc.Arguments{
				osc.String(arg.DefName),
				osc.Int(arg.ID),
				osc.Int(arg.Action),
				osc.Int(arg.Target),
			},
		}
		for k, v := range arg.Ctls {
			msg.Arguments = append(msg.Arguments, osc.String(k))
			msg.Arguments = append(msg.Arguments, osc.Float(v))
		}
		bun.Packets[i] = msg
	}
	return c.oscConn.Send(bun)
}

// Group creates a group.
func (c *Client) Group(id, action, target int32) (*GroupNode, error) {
	msg := osc.Message{
		Address: groupNewAddress,
		Arguments: osc.Arguments{
			osc.Int(id),
			osc.Int(action),
			osc.Int(target),
		},
	}
	if err := c.oscConn.Send(msg); err != nil {
		return nil, err
	}
	return newGroup(c, id), nil
}

// AddDefaultGroup adds the default group.
func (c *Client) AddDefaultGroup() (*GroupNode, error) {
	return c.Group(DefaultGroupID, AddToTail, RootNodeID)
}

// QueryGroup g_queryTree for a particular group.
func (c *Client) QueryGroup(id int32) (*GroupNode, error) {
	if err := c.oscConn.Send(osc.Message{
		Address: groupQueryTreeAddress,
		Arguments: osc.Arguments{
			osc.Int(id),
			osc.Int(1),
		},
	}); err != nil {
		return nil, err
	}
	// wait for response
	var resp osc.Message
	select {
	case resp = <-c.gqueryTreeChan:
	case <-time.After(2 * time.Second):
		return nil, errors.New("timeout waiting for response")
	}
	if numArgs := len(resp.Arguments); numArgs < 3 {
		return nil, fmt.Errorf("expected 3 arguments for message, got %d", numArgs)
	}
	// Throw away the flag that tells us we want to include synth controls in the reply.
	// We already know we requested that!
	resp.Arguments = resp.Arguments[1:]
	return c.parseGroup(resp)
}

// NextSynthID gets the next available ID for creating a synth
func (c *Client) NextSynthID() int32 {
	return atomic.AddInt32(&c.nextSynthID, 1)
}

// FreeAll frees all nodes in a group
func (c *Client) FreeAll(gids ...int32) error {
	msg := osc.Message{
		Address: groupFreeAllAddress,
	}
	for _, gid := range gids {
		msg.Arguments = append(msg.Arguments, osc.Int(gid))
	}
	return c.oscConn.Send(msg)
}

// addOscHandlers adds OSC handlers
func (c *Client) oscHandlers() osc.Dispatcher {
	return map[string]osc.MessageHandler{
		bufferInfoAddress: osc.Method(func(msg osc.Message) error {
			c.bufferInfoChan <- msg
			return nil
		}),
		statusReplyAddress: osc.Method(func(msg osc.Message) error {
			c.statusChan <- msg
			return nil
		}),
		doneOscAddress: osc.Method(func(msg osc.Message) error {
			c.doneChan <- msg
			return nil
		}),
		groupQueryTreeReplyAddress: osc.Method(func(msg osc.Message) error {
			c.gqueryTreeChan <- msg
			return nil
		}),
	}
}

// PlayDef plays a synthdef by sending the synthdef using
// DefaultClient, then immediately creating a synth node from the def.
func PlayDef(def *Synthdef) (*Synth, error) {
	c, err := DefaultClient()
	if err != nil {
		return nil, err
	}

	if err := c.SendDef(def); err != nil {
		return nil, err
	}

	synthID := c.NextSynthID()
	return defaultGroup.Synth(def.Name, synthID, AddToTail, nil)
}

// Close closes the client.
func (c *Client) Close() error {
	if c.isClosed() {
		return nil
	}
	atomic.StoreInt32(&c.closed, 1)
	if err := c.oscConn.Close(); err != nil {
		return err
	}
	close(c.errChan)
	close(c.doneChan)
	close(c.statusChan)
	close(c.gqueryTreeChan)
	return nil
}

// isClosed says whether or not the client is closed.
func (c *Client) isClosed() bool {
	return atomic.LoadInt32(&c.closed) == int32(1)
}
