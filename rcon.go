// Package rcon implements Source RCON Protocol which is described in the
// documentation: https://developer.valvesoftware.com/wiki/Source_RCON_Protocol.
package rcon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"time"
)

const (
	// DefaultDialTimeout provides default auth timeout to remote server.
	DefaultDialTimeout = 5 * time.Second

	// DefaultExecuteTimeout provides default timeout of the execute function to remote server.
	DefaultExecuteTimeout = 5 * time.Second

	// DefaultDeadline provides default deadline to tcp read/write operations.
	DefaultDeadline = 5 * time.Second

	// MaxCommandLen is an artificial restriction, but it will help in case of random
	// large queries.
	MaxCommandLen = 1000

	// SERVERDATA_AUTH is the first packet sent by the client,
	// which is used to authenticate the conn with the server.
	SERVERDATA_AUTH int32 = 3

	// SERVERDATA_AUTH_ID is any positive integer, chosen by the client
	// (will be mirrored back in the server's response).
	SERVERDATA_AUTH_ID int32 = 0

	// SERVERDATA_AUTH_RESPONSE packet is a notification of the conn's current auth
	// status. When the server receives an auth request, it will respond with an empty
	// SERVERDATA_RESPONSE_VALUE, followed immediately by a SERVERDATA_AUTH_RESPONSE
	// indicating whether authentication succeeded or failed. Note that the status
	// code is returned in the packet id field, so when pairing the response with
	// the original auth request, you may need to look at the packet id of the
	// preceding SERVERDATA_RESPONSE_VALUE.
	// If authentication was successful, the ID assigned by the request.
	// If auth failed, -1 (0xFF FF FF FF).
	SERVERDATA_AUTH_RESPONSE int32 = 2

	// SERVERDATA_RESPONSE_VALUE packet is the response to a SERVERDATA_EXECCOMMAND
	// request. The ID assigned by the original request.
	SERVERDATA_RESPONSE_VALUE int32 = 0

	// SERVERDATA_EXECCOMMAND packet type represents a command issued to the server
	// by a client. The response will vary depending on the command issued.
	SERVERDATA_EXECCOMMAND int32 = 2
)

var (
	// ErrInvalidAuthResponse is returned when we didn't get an auth packet
	// back for second read try after discard empty SERVERDATA_RESPONSE_VALUE
	// from authentication response.
	ErrInvalidAuthResponse = errors.New("invalid authentication packet type response")

	// ErrAuthFailed is returned when the package id from authentication
	// response is -1.
	ErrAuthFailed = errors.New("authentication failed")

	// ErrInvalidPacketID is returned when the package id from server response
	// was not mirrored back from request.
	ErrInvalidPacketID = errors.New("response for another request")

	// ErrInvalidPacketPadding is returned when the bytes after type field from
	// response is not equal to null-terminated ASCII strings.
	ErrInvalidPacketPadding = errors.New("invalid response padding")

	// ErrResponseTooSmall is returned when the server response is smaller
	// than 10 bytes.
	ErrResponseTooSmall = errors.New("response too small")

	// ErrCommandTooLong is returned when executed command length is bigger
	// than MaxCommandLen characters.
	ErrCommandTooLong = errors.New("command too long")

	// ErrCommandEmpty is returned when executed command length equal 0.
	ErrCommandEmpty = errors.New("command too small")

	// ErrMultiErrorOccurred is returned when close connection failed with
	// error after auth failed.
	ErrMultiErrorOccurred = errors.New("an error occurred while handling another error")

	// ErrExecuteTimeout is returned when Execute fn times out
	// error after Execute times out
	ErrExecuteTimeout = errors.New("excecute timeout")

	// ErrConnectionNotOpen is retuned when connection not open
	// error after connection is not open
	ErrConnectionNotOpen = errors.New("connection not open")
)

// Conn is source RCON generic stream-oriented network connection.
type Conn struct {
	conn         net.Conn
	settings     Settings
	openExecutes map[int32]chan *Packet
	stream       chan *Packet
	connected    bool
}

// Dial creates a new authorized Conn tcp dialer connection.
func Dial(address string, password string, options ...Option) (*Conn, error) {
	settings := DefaultSettings

	for _, option := range options {
		option(&settings)
	}

	conn, err := net.DialTimeout("tcp", address, settings.dialTimeout)
	if err != nil {
		// Failed to open TCP connection to the server.
		return nil, fmt.Errorf("rcon: %w", err)
	}

	client := Conn{conn: conn, settings: settings, stream: make(chan *Packet, 100)}

	if err := client.auth(password); err != nil {
		// Failed to auth conn with the server.
		if err2 := client.Close(); err2 != nil {
			//nolint:errorlint // TODO: Come up with the better wrapping
			return &client, fmt.Errorf("%w: %v. Previous error: %v", ErrMultiErrorOccurred, err2, err)
		}

		return &client, fmt.Errorf("rcon: %w", err)
	}

	client.openExecutes = make(map[int32]chan *Packet)
	client.connected = true
	//main reading routine
	go func() {
		for client.openExecutes != nil {
			packet, err := client.read()
			if err != nil {
				if err == io.EOF {
					if len(client.stream) >= 100 {
						<-client.stream
					}
					client.stream <- nil
					break
				}
				continue
			}
			if len(client.stream) >= 100 {
				<-client.stream
			}
			client.stream <- packet
			for _, value := range client.openExecutes {
				go func(v chan *Packet) {
					v <- packet
				}(value)
			}

		}
	}()

	return &client, nil
}

// Execute sends command type and it string to execute to the remote server,
// creating a packet with a SERVERDATA_EXECCOMMAND_ID for the server to mirror,
// and compiling its payload bytes in the appropriate order. The response body
// is decompiled from bytes into a string for return.
func (c *Conn) Execute(command string) (string, error) {

	if !c.connected {
		return "", ErrConnectionNotOpen
	}

	randId := int32(rand.Int())

	if command == "" {
		return "", ErrCommandEmpty
	}

	if len(command) > MaxCommandLen {
		return "", ErrCommandTooLong
	}
	c.openExecutes[int32(randId)] = make(chan *Packet)

	if err := c.write(SERVERDATA_EXECCOMMAND, randId, command); err != nil {
		delete(c.openExecutes, randId)
		return "", err
	}

	var response *Packet
	var err error
	timeout := time.After(c.settings.executeTimeout)
loop:
	for {
		select {
		case <-timeout:
			err = ErrExecuteTimeout
			response = nil
			break loop
		case response = <-c.openExecutes[int32(randId)]:
			if response != nil && response.ID == randId {
				break loop
			}
		}
	}
	delete(c.openExecutes, randId)
	if response == nil {
		return "", err
	}
	return response.Body(), err
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Close closes the connection.
func (c *Conn) Close() error {
	c.connected = false
	return c.conn.Close()
}

// Get all rcon packet output as channel. Useful for console implementations, where you are not waiting for a specific package
func (c *Conn) Read() (*Packet, error) {
	if !c.connected {
		return nil, ErrConnectionNotOpen
	}
	pck := <-c.stream
	if pck == nil {
		c.Close()
		return nil, io.EOF
	}
	return pck, nil
}

// auth sends SERVERDATA_AUTH request to the remote server and
// authenticates client for the next requests.
func (c *Conn) auth(password string) error {
	if err := c.write(SERVERDATA_AUTH, SERVERDATA_AUTH_ID, password); err != nil {
		return err
	}

	if c.settings.deadline != 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.settings.deadline)); err != nil {
			return err
		}
	}

	response, err := c.readHeader()
	if err != nil {
		return err
	}

	// When the server receives an auth request, it will respond with an empty
	// SERVERDATA_RESPONSE_VALUE, followed immediately by a SERVERDATA_AUTH_RESPONSE
	// indicating whether authentication succeeded or failed.
	// Some servers doesn't send an empty SERVERDATA_RESPONSE_VALUE packet, so we
	// do this case optional.
	if response.Type == SERVERDATA_RESPONSE_VALUE {
		// Discard empty SERVERDATA_RESPONSE_VALUE from authentication response.
		_, _ = c.conn.Read(make([]byte, response.Size-PacketHeaderSize))

		if response, err = c.readHeader(); err != nil {
			return err
		}
	}

	// We must to read response body.
	buffer := make([]byte, response.Size-PacketHeaderSize)
	if _, err := c.conn.Read(buffer); err != nil {
		return err
	}

	if response.Type != SERVERDATA_AUTH_RESPONSE {
		return ErrInvalidAuthResponse
	}

	if response.ID == -1 {
		return ErrAuthFailed
	}

	if response.ID != SERVERDATA_AUTH_ID {
		return ErrInvalidPacketID
	}

	return nil
}

// write creates packet and writes it to established tcp conn.
func (c *Conn) write(packetType int32, packetID int32, command string) error {
	if c.settings.deadline != 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(c.settings.deadline)); err != nil {
			return fmt.Errorf("rcon: %w", err)
		}
	}

	packet := NewPacket(packetType, packetID, command)
	_, err := packet.WriteTo(c.conn)

	return err
}

// read reads structured binary data from c.conn into packet.
func (c *Conn) read() (*Packet, error) {
	if c.settings.deadline != 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.settings.deadline)); err != nil {
			return nil, err
		}
	}

	packet := &Packet{}
	if _, err := packet.ReadFrom(c.conn); err != nil {
		return packet, err
	}

	return packet, nil
}

// readHeader reads structured binary data without body from c.conn into packet.
func (c *Conn) readHeader() (Packet, error) {
	var packet Packet
	if err := binary.Read(c.conn, binary.LittleEndian, &packet.Size); err != nil {
		return packet, err
	}

	if err := binary.Read(c.conn, binary.LittleEndian, &packet.ID); err != nil {
		return packet, err
	}

	if err := binary.Read(c.conn, binary.LittleEndian, &packet.Type); err != nil {
		return packet, err
	}

	return packet, nil
}
