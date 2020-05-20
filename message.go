package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Constants //

// ChokeID means the server is choking the client
const ChokeID byte = 0

// UnchokeID means the server isn't choking the client
const UnchokeID byte = 1

// InterestedID means the client is interested in the server
const InterestedID byte = 2

// UninterestedID means the client isn't interested in the server
const UninterestedID byte = 3

// HaveID means the client has successfully recieved and verified a piece from the server
const HaveID byte = 4

// BitfieldID means the server's sent a bitfield representing which pieces it has to the client
const BitfieldID byte = 5

// RequestID means the client is requesting a block from the server
const RequestID byte = 6

// PieceID means the server has a block for the client
const PieceID byte = 7

// CancelID means the client doesn't need a block from the server anymore
const CancelID byte = 8

// PortID means the server is listening on this port for DHT
const PortID byte = 9

// ExtendedProtocolID means this message uses an extension to BitTorrent
const ExtendedProtocolID byte = 20

// Structs //

// Message represents the content of a BitTorrent message
type Message struct {
	ID      byte
	content []byte
}

func (message *Message) encode() []byte {
	if message == nil { // if there's no message, per specifications we should assume it's keep-alive...
		return make([]byte, 4) // ...and send a msg of 4 zeroes (length = 0)
	}

	messageLength := uint32(1 + len(message.content)) // 1 byte for the ID, n bytes for the content
	messageBytes := make([]byte, (4 + messageLength)) // 4 bytes for the length itself, messageLength bytes for everything else

	binary.BigEndian.PutUint32(messageBytes[:4], messageLength)
	messageBytes[4] = message.ID
	copy(messageBytes[5:], message.content) // POSSIBLE BUFFER OVERFLOW?????????

	return messageBytes
}

func (peer *Peer) sendRequest(pieceIndex uint32, bytesBeginIndex uint32, length uint32) error {
	requestMessage := Message{
		ID:      RequestID,
		content: make([]byte, 12), // 4 bytes for each uint32 argument
	}
	binary.BigEndian.PutUint32(requestMessage.content[0:4], pieceIndex)
	binary.BigEndian.PutUint32(requestMessage.content[4:8], bytesBeginIndex)
	binary.BigEndian.PutUint32(requestMessage.content[8:12], length)

	_, err := peer.tcpConnection.Write(requestMessage.encode())
	return err
}

func (peer *Peer) recieveAndDecodeMessage() (*Message, error) {
	var message Message

	peer.tcpConnection.SetDeadline(time.Now().Add(PieceTimeout))
	defer peer.tcpConnection.SetDeadline(time.Time{}) // per protocol, deadlines should be set per transaction, so reset the deadline when we're done here
	lengthBytes := make([]byte, 4)
	_, err := io.ReadFull(peer.tcpConnection, lengthBytes)
	if err != nil {
		fmt.Printf("error reading header from %s: %s\n", peer.peerAddress.IP, err)
		return nil, err
	}

	messageLength := binary.BigEndian.Uint32(lengthBytes)
	if messageLength == 0 { // 0-length messages are keep-alives; we don't need to look any further into them
		return nil, nil
	}

	messageBytes := make([]byte, binary.BigEndian.Uint32(lengthBytes))
	_, err = io.ReadFull(peer.tcpConnection, messageBytes)
	if err != nil {
		fmt.Printf("error reading message from %s: %s\n", peer.peerAddress.IP, err)
		return nil, err
	}

	message.ID = messageBytes[0]
	message.content = messageBytes[1:]

	return &message, nil
}

func (peer *Peer) sendMessage(messageID byte, messageContent []byte) error {
	message := Message{messageID, messageContent}
	_, err := peer.tcpConnection.Write(message.encode())
	return err
}

func (peer *Peer) sendExtendedMessage(protocolID byte, messageContent []byte) error {
	return peer.sendMessage(ExtendedProtocolID, append([]byte{protocolID}, messageContent...)) // extended protocol messages have a header format [4 bytes of length][byte 20][byte identifying extended protocol]
}

func (peer *Peer) sendChoke() error {
	return peer.sendMessage(ChokeID, nil)
}

func (peer *Peer) sendInterested() error {
	return peer.sendMessage(InterestedID, nil)
}

func (peer *Peer) sendUnchoke() error {
	return peer.sendMessage(UnchokeID, nil)
}

func (peer *Peer) sendUninterested() error {
	return peer.sendMessage(UninterestedID, nil)
}

func (peer *Peer) sendHave(index uint32) error {
	indexEncoded := make([]byte, 4)
	binary.BigEndian.PutUint32(indexEncoded, index)
	return peer.sendMessage(HaveID, indexEncoded)
}
