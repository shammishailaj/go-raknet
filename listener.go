package raknet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Listener implements a RakNet connection listener. It follows the same methods as those implemented by the
// TCPListener in the net package.
// Listener implements the net.Listener interface.
type Listener struct {
	conn     net.PacketConn
	settings []Setting

	// incoming is a channel of incoming connections. Connections that end up in here will also end up in
	// the connections map.
	incoming chan *Conn

	// connections is a map of currently active connections, indexed by their address.
	connections sync.Map

	close chan bool

	// id is a random server ID generated upon starting listening. It is used several times throughout the
	// connection sequence of RakNet.
	id int64

	// pongData is a byte slice of data that is sent in an unconnected pong packet each time the client sends
	// and unconnected ping to the server.
	pongData atomic.Value

	// protocol is the RakNet protocol of the listener.
	protocol byte
}

// Listen listens on the address passed and returns a listener that may be used to accept connections. If not
// successful, an error is returned.
// The address follows the same rules as those defined in the net.TCPListen() function.
func Listen(address string, settings ...Setting) (*Listener, error) {
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, fmt.Errorf("error creating UDP listener: %v", err)
	}

	// Seed the global rand so we can get a random ID.
	rand.Seed(time.Now().Unix())
	listener := &Listener{
		conn:     conn,
		settings: settings,
		incoming: make(chan *Conn, 128),
		close:    make(chan bool, 1),
		id:       rand.Int63(),
		protocol: MinecraftProtocol,
	}
	settingMap := mapSettings(settings)
	if v, ok := settingMap[version]; ok {
		listener.protocol = v.(byte)
	}

	listener.pongData.Store([]byte{})
	go listener.listen()
	return listener, nil
}

// Accept blocks until a connection can be accepted by the listener. If successful, Accept returns a
// connection that is ready to send and receive data. If not successful, a nil listener is returned and an error
// describing the problem.
func (listener *Listener) Accept() (net.Conn, error) {
accept:
	conn, ok := <-listener.incoming
	if !ok {
		return nil, fmt.Errorf("error accepting connection: listener closed")
	}
	select {
	case <-conn.finishedSequence:
		go func() {
			<-conn.close
			// Insert the boolean back in the channel so that other readers of the channel also receive
			// the signal.
			conn.close <- true
			listener.connections.Delete(conn.addr.String())
		}()
		return conn, nil
	case <-time.After(time.Second * 10):
		// It took too long to complete this connection. We close it and go back to accepting.
		_ = conn.Close()
		goto accept
	}
}

// Addr returns the address the Listener is bound to and listening for connections on.
func (listener *Listener) Addr() net.Addr {
	return listener.conn.LocalAddr()
}

// Close closes the listener so that it may be cleaned up. It makes sure the goroutine handling incoming
// packets is able to be freed.
func (listener *Listener) Close() error {
	if len(listener.close) != 0 {
		// The listener was already closed, no need to do it again.
		return nil
	}
	listener.close <- true
	var err error
	listener.connections.Range(func(key, value interface{}) bool {
		conn := value.(*Conn)
		if closeErr := conn.Close(); err != nil {
			err = fmt.Errorf("error closing conn %v: %v", conn.addr, closeErr)
		}
		return true
	})
	if err != nil {
		return err
	}
	close(listener.incoming)
	if err := listener.conn.Close(); err != nil {
		return fmt.Errorf("error closing UDP listener: %v", err)
	}
	return nil
}

// PongData sets the pong data that is used to respond with when a client sends a ping. It usually holds game
// specific data that is used to display in a server list.
// If a data slice is set with a size bigger than math.MaxInt16, the function panics.
func (listener *Listener) PongData(data []byte) {
	if len(data) > math.MaxInt16 {
		panic(fmt.Sprintf("error setting pong data: pong data must not be longer than %v", math.MaxInt16))
	}
	listener.pongData.Store(data)
}

// HijackPong hijacks the pong response from a server at an address passed. The listener passed will
// continuously update its pong data by hijacking the pong data of the server at the address.
// The hijack will last until the listener is shut down.
// If the address passed could not be resolved, an error is returned.
// Calling HijackPong means that any current and future pong data set using listener.PongData is overwritten
// each update.
// A list of settings may be passed in to specify additional settings such as the protocol version for the
// ping/pong.
func (listener *Listener) HijackPong(address string) error {
	if _, err := net.ResolveUDPAddr("udp", address); err != nil {
		return fmt.Errorf("error resolving UDP address: %v", err)
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				data, err := Ping(address, listener.settings...)
				if err != nil {
					// It's okay if these packets are lost sometimes. There's no need to log this.
					continue
				}
				if string(data[:4]) == "MCPE" {
					fragments := bytes.Split(data, []byte{';'})
					for len(fragments) < 9 {
						// Append to the fragments if it's not at least 9 elements long.
						fragments = append(fragments, nil)
					}

					fragments = fragments[:9]
					fragments[6] = []byte(strconv.Itoa(int(listener.id)))
					fragments[7] = []byte("Proxy")
					fragments[8] = []byte{}

					listener.PongData(bytes.Join(fragments, []byte{';'}))
				} else {
					listener.PongData(data)
				}
			case <-listener.close:
				// Add another value to the channel so that other listeners can listen for the closing of the
				// listener too.
				listener.close <- true
				return
			}
		}
	}()
	return nil
}

// ID returns the unique ID of the listener. This ID is usually used by a client to identify a specific
// server during a single session.
func (listener *Listener) ID() int64 {
	return listener.id
}

// listen continuously reads from the listener's UDP connection, until close has a value in it.
func (listener *Listener) listen() {
	// Create a buffer with the maximum size a UDP packet sent over RakNet is allowed to have. We can re-use
	// this buffer for each packet.
	b := make([]byte, 1500)
	for {
		if len(listener.close) == 1 {
			// Stop the function so that any goroutine that is running it is able to be cleaned up.
			return
		}
		n, addr, err := listener.conn.ReadFrom(b)
		if err != nil {
			log.Printf("error reading from UDP connection (rakAddr = %v): %v", addr, err)
			continue
		}
		buffer := b[:n]

		// Technically we should not re-use the same byte slice after its ownership has been taken by the
		// buffer, but we can do this anyway because we copy the data later.
		if err := listener.handle(bytes.NewBuffer(buffer), addr); err != nil {
			log.Printf("error handling packet (rakAddr = %v): %v\n", addr, err)
		}
	}
}

// handle handles an incoming packet in buffer b from the address passed. If not successful, an error is
// returned describing the issue.
func (listener *Listener) handle(b *bytes.Buffer, addr net.Addr) error {
	value, found := listener.connections.Load(addr.String())
	if !found {
		// If there was no session yet, it means the packet is an offline message. It is not contained in a
		// datagram.
		packetID, err := b.ReadByte()
		if err != nil {
			return fmt.Errorf("error reading packet ID byte: %v", err)
		}
		switch packetID {
		case idUnconnectedPing:
			return listener.handleUnconnectedPing(b, addr)
		case idOpenConnectionRequest1:
			return listener.handleOpenConnectionRequest1(b, addr)
		case idOpenConnectionRequest2:
			return listener.handleOpenConnectionRequest2(b, addr)
		default:
			// In some cases, the client will keep trying to send datagrams while it has already timed out. In
			// this case, we should not print an error.
			if packetID&bitFlagValid == 0 {
				return fmt.Errorf("unknown packet received (%x): %x", packetID, b.Bytes())
			}
		}
		return nil
	}
	conn := value.(*Conn)
	return conn.receive(b)
}

// handleOpenConnectionRequest2 handles an open connection request 2 packet stored in buffer b, coming from
// an address addr.
func (listener *Listener) handleOpenConnectionRequest2(b *bytes.Buffer, addr net.Addr) error {
	packet := &openConnectionRequest2{}
	if err := packet.UnmarshalBinary(b.Bytes()); err != nil {
		return fmt.Errorf("error reading open connection request 2: %v", err)
	}
	b.Reset()

	address := rakAddr(*addr.(*net.UDPAddr))
	response := &openConnectionReply2{Magic: magic, ServerGUID: listener.id, ClientAddress: &address, MTUSize: packet.MTUSize}
	if err := b.WriteByte(idOpenConnectionReply2); err != nil {
		return fmt.Errorf("error writing open connection reply 2 ID: %v", err)
	}
	data, err := response.MarshalBinary()
	if err != nil {
		return fmt.Errorf("error writing open connection reply 2: %v", err)
	}
	if _, err := b.Write(data); err != nil {
		return fmt.Errorf("error writing open connection reply 2 to buffer: %v", err)
	}
	if _, err := listener.conn.WriteTo(b.Bytes(), addr); err != nil {
		return fmt.Errorf("error sending open connection reply 2: %v", err)
	}

	conn := newConn(listener.conn, addr, packet.MTUSize, packet.ClientGUID)
	listener.connections.Store(addr.String(), conn)
	// Add the connection to the incoming channel so that a caller of Accept() can receive it.
	listener.incoming <- conn

	return nil
}

// handleOpenConnectionRequest1 handles an open connection request 1 packet stored in buffer b, coming from
// an address addr.
func (listener *Listener) handleOpenConnectionRequest1(b *bytes.Buffer, addr net.Addr) error {
	// mtuSize is the total size of the buffer. We already read the packet ID byte, so we need to add that to
	// the size.
	mtuSize := len(b.Bytes()) + 1

	packet := &openConnectionRequest1{}
	if err := binary.Read(b, binary.BigEndian, packet); err != nil {
		return fmt.Errorf("error reading open connection request 1: %v", err)
	}
	b.Reset()

	if packet.Protocol != listener.protocol {
		response := &incompatibleProtocolVersion{Magic: magic, ServerGUID: listener.id, ServerProtocol: listener.protocol}
		if err := b.WriteByte(idIncompatibleProtocolVersion); err != nil {
			return fmt.Errorf("error writing incompatible protocol version ID: %v", err)
		}
		if err := binary.Write(b, binary.BigEndian, response); err != nil {
			return fmt.Errorf("error writing incompatible protocol version: %v", err)
		}
		if _, err := listener.conn.WriteTo(b.Bytes(), addr); err != nil {
			return fmt.Errorf("error sending incompatible protocol version: %v", err)
		}
		return fmt.Errorf("error handling open connection request 1: incompatible protocol version %v (listener protocol = %v)", packet.Protocol, listener.protocol)
	}

	response := &openConnectionReply1{Magic: magic, ServerGUID: listener.id, MTUSize: int16(mtuSize) + 28}
	if err := b.WriteByte(idOpenConnectionReply1); err != nil {
		return fmt.Errorf("error writing open connection reply 1 ID: %v", err)
	}
	if err := binary.Write(b, binary.BigEndian, response); err != nil {
		return fmt.Errorf("error writing open connection reply 1: %v", err)
	}
	if _, err := listener.conn.WriteTo(b.Bytes(), addr); err != nil {
		return fmt.Errorf("error sending open connection reply 1: %v", err)
	}
	return nil
}

// handleUnconnectedPing handles an unconnected ping packet stored in buffer b, coming from an address addr.
func (listener *Listener) handleUnconnectedPing(b *bytes.Buffer, addr net.Addr) error {
	packet := &unconnectedPing{}
	if err := binary.Read(b, binary.BigEndian, packet); err != nil {
		return fmt.Errorf("error reading unconnected ping: %v", err)
	}
	b.Reset()

	pongData := listener.pongData.Load().([]byte)
	response := &unconnectedPong{Magic: magic, ServerGUID: listener.id, SendTimestamp: timestamp()}
	if err := b.WriteByte(idUnconnectedPong); err != nil {
		return fmt.Errorf("error writing unconnected pong ID: %v", err)
	}
	if err := binary.Write(b, binary.BigEndian, response); err != nil {
		return fmt.Errorf("error writing unconnected pong: %v", err)
	}
	if listener.protocol == MinecraftProtocol {
		if err := binary.Write(b, binary.BigEndian, int16(len(pongData))); err != nil {
			return fmt.Errorf("error writing unconnected pong data length")
		}
	}
	if _, err := b.Write(pongData); err != nil {
		return fmt.Errorf("error writing pong data to buffer: %v", err)
	}
	if _, err := listener.conn.WriteTo(b.Bytes(), addr); err != nil {
		return fmt.Errorf("error sending unconnected pong: %v", err)
	}
	return nil
}

// timestamp returns a timestamp in milliseconds.
func timestamp() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}
