package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"time"
)

// "Constants" but some of them aren't constant types //

// ConnectionID is the byte sequence that identifies a BitTorrent UDP connection packet
var ConnectionID = [8]byte{0x00, 0x00, 0x04, 0x17, 0x27, 0x10, 0x19, 0x80}

// EightByteZero is exactly what it sounds like (used in the UDP announce message)
var EightByteZero = [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// UDPNumPeers is the the number of peers we request over the UDP connection; it's the same as the number of peers requested over HTTP, but big-endian encoded already
var UDPNumPeers = [8]byte{0x00, 0x00, 0x00, 0x32} // = 50

// UDPPort is the port we tell the tracker we're listening on; it's the same as the HTTP listening port, but big-endian encoded already
var UDPPort = [8]byte{0x1a, 0xe6} // = 6886

// UDPTrackerTimeout is how long we wait for the tracker's response before we give up and try again
const UDPTrackerTimeout = time.Second * 3

// UDPAnnounceID is the code used in the UDP announcement handshake
var UDPAnnounceID = [4]byte{0x00, 0x00, 0x00, 0x01}

func (torrent *Torrent) connectToTrackerUDP() ([]PeerAddress, error) {
	transactionID := make([]byte, 4) // generate a random transaction ID that'll be used to identify the client during this handshake
	rand.Read(transactionID)

	udpConnection, err := net.DialTimeout("udp", torrent.torrentData.announceURL[6:], UDPTrackerTimeout) // dialing like this on every connection attempt is slightly inefficient | 6: on the url to avoid a very stupid URL parsing error
	if err != nil {
		return nil, err
	}
	defer udpConnection.Close()

	connectionID, err := trackerConnectUDP(udpConnection, transactionID) // send and recieve the connect message, the first half of the handshake
	if err != nil {
		return nil, err
	}

	return torrent.trackerAnnounceUDP(udpConnection, transactionID, connectionID) // return the result of the announce message and reply, the second half of the handshake
}

func trackerConnectUDP(connection net.Conn, transactionID []byte) ([]byte, error) { // using a []byte is a space > time tradeoff with using a uint64, which would be less bytes but also require us to do big endian encoding every message
	connectMessage := make([]byte, 16)
	copy(connectMessage[:8], ConnectionID[:])
	// the 8, 9, 10, and 11 bytes are left zero to indicate the type of this message is 0 (a.k.a. Connect)
	copy(connectMessage[12:], transactionID)

	_, err := connection.Write(connectMessage)
	if err != nil {
		return nil, err
	}

	// reuse the connectMessage buffer to store the response, which is also 16 bytes
	connection.SetDeadline(time.Now().Add(UDPTrackerTimeout))
	defer connection.SetDeadline(time.Time{}) // per protocol, deadlines should be set per transaction, so reset the deadline when we're done here
	_, err = connection.Read(connectMessage)
	if err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(connectMessage[:4]) != 0 { // if the response wasn't a Connect message (usually otherwise has ID = 3, which is error)
		return nil, fmt.Errorf("udp connection to tracker failed: %s", connectMessage[4:])
	}
	if bytes.Compare(connectMessage[4:8], transactionID) != 0 { // if the reply had an incorrect transactionID, this transaction is corrupted (probably)
		return nil, fmt.Errorf("udp connection to tracker failed; incorrect transactionID")
	}

	// if we've gotten this far, then the next 8 bytes in the connect reply will be the connectionID, which we need for the announce message
	return connectMessage[8:16], nil
}

func (torrent *Torrent) trackerAnnounceUDP(connection net.Conn, transactionID []byte, connectionID []byte) ([]PeerAddress, error) {
	bytesLeftToDownload := make([]byte, 8) // define some constants used in the handshake
	binary.BigEndian.PutUint64(bytesLeftToDownload, torrent.torrentData.infoDict.totalLength)
	uniqueKey := make([]byte, 4) // a thoroughly useless unique key
	rand.Read(uniqueKey)

	announceMessage := make([]byte, 100)
	torrent.populateAnnounceMessage(announceMessage, transactionID, connectionID, bytesLeftToDownload, uniqueKey)
	_, err := connection.Write(announceMessage)
	if err != nil {
		return nil, err
	}

	connection.SetDeadline(time.Now().Add(UDPTrackerTimeout))
	replyData := make([]byte, 1024)
	announceReplyBytes, err := connection.Read(replyData)
	if err != nil {
		return nil, err
	}
	if announceReplyBytes < 20 {
		return nil, fmt.Errorf("udp announcement wasn't long enough (expected 20 bytes, got %d bytes)", announceReplyBytes)
	}
	if binary.BigEndian.Uint32(replyData[:4]) != 1 { // if the response wasn't an Announce message (usually otherwise has ID = 3, which is error)
		return nil, fmt.Errorf("udp announcement to tracker failed: %s", replyData[4:])
	}
	if bytes.Compare(replyData[4:8], transactionID) != 0 { // if the reply had an incorrect transactionID, this transaction is corrupted (probably)
		return nil, fmt.Errorf("udp announcement to tracker failed; incorrect transactionID")
	}

	torrent.trackerConnection.interval = binary.BigEndian.Uint32(replyData[8:12])
	torrent.trackerConnection.incomplete = binary.BigEndian.Uint32(replyData[12:16])
	torrent.trackerConnection.complete = binary.BigEndian.Uint32(replyData[16:20])

	peerAddresses := make([]PeerAddress, 50) // 50 is the hardcoded number of peers we want
	nPeers := 0
	connection.SetDeadline(time.Now().Add(UDPTrackerTimeout))
	for dataIndex := 20; nPeers < 50; dataIndex += 6 { // read the next 6 bytes and convert them to a PeerAddress until a) we've reached 50 peer addresses or b) there's no more data (i.e. we were sent less than 50 peers)
		if bytes.Compare(replyData[dataIndex:(dataIndex+4)], []byte{0, 0, 0, 0}) == 0 { // finish reading when there's no more peers, so we can know how many peers to add later [YET TO IMPLEMENT]
			break
		}
		peerAddresses[nPeers].IP = net.IP(replyData[dataIndex:(dataIndex + 4)])
		peerAddresses[nPeers].port = binary.BigEndian.Uint16(replyData[(dataIndex + 4):(dataIndex + 6)])
		nPeers++
	}

	// if we've gotten this far, everything worked out okay
	return peerAddresses, nil
}

func (torrent *Torrent) populateAnnounceMessage(announceMessage []byte, transactionID []byte, connectionID []byte, bytesLeftToDownload []byte, uniqueKey []byte) {
	copy(announceMessage[:8], connectionID)
	copy(announceMessage[8:12], UDPAnnounceID[:])
	copy(announceMessage[12:16], transactionID)
	copy(announceMessage[16:36], torrent.torrentData.infoDict.sha1Hash[:])
	copy(announceMessage[36:56], torrent.client.peerID[:])
	copy(announceMessage[56:64], EightByteZero[:]) // how many bytes we've downloaded so far
	copy(announceMessage[64:72], bytesLeftToDownload)
	copy(announceMessage[72:80], EightByteZero[:]) // how many bytes we've uploaded so far
	// bytes 80, 81, 82, and 83 left blank for no event, bytes 84, 85, 86, and 87 left to tell the tracker to use our IP
	copy(announceMessage[88:92], uniqueKey)
	copy(announceMessage[92:96], UDPNumPeers[:])
	copy(announceMessage[96:98], UDPPort[:])
	// bytes 98 and 99 left blank since we're not using any extensions
}
