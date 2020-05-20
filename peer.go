package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"time"

	"github.com/IncSW/go-bencode"
)

// Constants //

// ExtendedRequestID is the ID transmitted with the extended shake
const ExtendedRequestID = 20

// InfoDictRequestID indicates a message requesting part of an infoDict over the BEP 9 protocol
const InfoDictRequestID = uint64(0)

// InfoDictDataID indicates a message carrying part of an infoDict, in response to an InfoDictRequest message
const InfoDictDataID = uint64(1)

// InfoDictRejectID indicates that a client doesn't have the metadata requested in an InfoDictRequest message
const InfoDictRejectID = uint64(2)

// LocalMetadataID is our client's ID for ut_metadata
const LocalMetadataID = uint8(1)

// MaxFailuresPerPeer is the maximum number of times our connection to a peer can fail before we drop it
const MaxFailuresPerPeer = 10

// MaxPieceSize is the maximum bytes to download in one block (docs say 16kb, internet says 64kb, decided to play it safe)
const MaxPieceSize = 16384 // 1024 * 64

// MaxMetadataSize is the maximum bytes to accept as the size of the infoDict through BEP 9-style metadata exchange
const MaxMetadataSize = 2000000

// MaxPeers is the number of peers the server intends to have for each torrent
const MaxPeers = 85

// MaxPendingRequests is the number of block requests we queue up before actually sending a TCP message
const MaxPendingRequests = 10

// MinBlockLen is the minimum number of bytes a block can contain
const MinBlockLen = 8

// PeerFailureBaseSleepTime is the number of seconds we sleep after each peer failure (multiplied by the number of failures there've been)
const PeerFailureBaseSleepTime = 5

// PeerTimeout is the time (in seconds) each peer is given to connect before it's dropped
const PeerTimeout = 5 * time.Second

// PieceTimeout is the time we wait for a piece to download before we give up and try again from a different peer
const PieceTimeout = 60 * time.Second

// "Constants" but they aren't constant datatypes //

// PeerConnectionFlagsExtended holds the flags we send to tell peers we support extended messages (used when we need to get torrent metadata from peers instead of .torrent files)
var PeerConnectionFlagsExtended = [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x00} // setting the 6th byte to 0x10 signals that we support extensions (namely, BEP 9)

// PeerConnectionFlagsDefault contains the default, empty, flags we send to every peer
var PeerConnectionFlagsDefault = [8]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// Structs //

// PeerAddress stores the contact info of a peer
type PeerAddress struct {
	IP   net.IP
	port uint16
}

// Peer stores all data necessary to transfer data to and from a peer
type Peer struct {
	bitfield             []byte
	choked               bool
	metadataExtensionID  uint8
	peerAddress          PeerAddress
	peerID               [20]byte
	reportedMetadataSize int64 // this could've been made more efficient by having peer.connect push a struct, but that struct would've stuck had to include this peer, so it'd stick around for as long as this peer did anyway
	supportsExtended     bool
	tcpConnection        net.Conn
}

// Handshake stores information that we use to make a connection to the peer
type Handshake struct {
	// pstr = 'BitTorrent protocol' and pstrlen = 19b are hardcoded into the encoding for this, but could be easily added if more protocols were needed
	flags    [8]byte // this is an array because a slice would actually take up more space (8 bytes for the pointer, 4 for the length, 4 for the cap)
	infoHash []byte  // this is a slice so we can use references to the pre-computed info hash in the Torrent
	peerID   [20]byte
}

func (handshake *Handshake) encode() []byte {
	payload := make([]byte, 49+ProtocolLength)

	payload[0] = ProtocolLength
	payloadIndex := 1
	payloadIndex += copy(payload[payloadIndex:], ProtocolName)                           // safe copy, since source is hardcoded
	payloadIndex += copy(payload[payloadIndex:], handshake.flags[:])                     // safe copy, since length is hardcoded
	payloadIndex += copy(payload[payloadIndex:(payloadIndex+20)], handshake.infoHash[:]) // safe copy, since only 20 bytes can be copied
	payloadIndex += copy(payload[payloadIndex:], handshake.peerID[:])                    // safe copy, since length is hardcoded

	return payload
}

func (handshake *Handshake) init(infoHash []byte, flags [8]byte) {
	rand.Read(handshake.peerID[:])     // make the peer ID another 20 random bytes
	handshake.infoHash = infoHash      // store a pointer to the infoHash of pieces, already computed by the Torrent
	copy(handshake.flags[:], flags[:]) // safe copy because flags is hardcoded as 8 bytes long
}

// Gets a peer's response to a handshake and confirms it has the right version + file(s), then return its peerID (if successful)
func (peer *Peer) decodeResponseAndVerify(infoHash []byte) ([]byte, error) {
	protocolSpecs := make([]byte, 1+ProtocolLength) // read the bytes corresponding to the protocol length & version first, so we'll know if the response is even valid
	_, err := io.ReadFull(peer.tcpConnection, protocolSpecs)
	if err != nil {
		return nil, err
	}

	if (protocolSpecs[0] != ProtocolLength) || (string(protocolSpecs[1:]) != ProtocolName) { // if the response has the wrong protocol, then the peer definitely isn't valid
		return nil, fmt.Errorf("unsupported protocol")
	}

	handshakeData := make([]byte, 28) // if the version is correct, read the remaining 8 option bytes and the 20 byte info hash (ignoring the 20 byte peerID because it might not be necessary)
	_, err = io.ReadFull(peer.tcpConnection, handshakeData)
	if err != nil {
		return nil, err
	}

	if (handshakeData[5] & 0x10) > 0 { // check whether the peer supports extensions (most importantly, this means we can ask them for the torrent metadata)
		peer.supportsExtended = true
	}

	if !bytes.Equal(handshakeData[8:28], infoHash) { // if the infoHash the peer responded with isn't the infoHash of the file(s) we want to download, then we don't want the peer
		return nil, fmt.Errorf("incorrect info hash")
	}

	remotePeerID := make([]byte, 20)                       // if we've gotten this far, we can return the remote peer's peerID...
	_, err = io.ReadFull(peer.tcpConnection, remotePeerID) // ...which is the last 20 bytes left from the handshake
	if err != nil {
		return nil, err
	}

	// if we've gotten this far, the version and hash check out | if for some reason the peer ID is needed, it could be returned here instead of a bool
	return remotePeerID, nil
}

// Checks a peer's bitfield to see whether they have a given piece
func (peer *Peer) getHasPiece(pieceIndex uint32) bool { // uint32 shouldn't be a problem unless you're torrenting the library of congress
	indexOfByte := pieceIndex / 8       // e.g. the status of the 15th piece is stored in the byte with index 1...
	indexInByte := 7 - (pieceIndex % 8) // ...at the byte of index (7 - 7) = 0 (since bytes are stored in big endian)

	if indexOfByte > uint32(len(peer.bitfield)) { // if it's an invalid piece index, we definintely don't have it
		return false
	}
	return ((peer.bitfield[indexOfByte] >> indexInByte) & 1) == 1
}

// Sets a peer's bitfield to indicate that they have a given piece
func (peer *Peer) setHasPiece(pieceIndex uint32) {
	indexOfByte := pieceIndex / 8       // e.g. the status of the 15th piece is stored in the byte with index 1...
	indexInByte := 7 - (pieceIndex % 8) // ...at the byte of index (7 - 7) = 0 (since bytes are stored in big endian)

	if indexOfByte > uint32(len(peer.bitfield)) { // if it's an invalid piece index, we definintely don't have it
		return
	}

	peer.bitfield[indexOfByte] = peer.bitfield[indexOfByte] | (1 << indexInByte)
}

// Return a peer's IP:Port address in a string that can be passed to DialTimeout()
func (peerAddress *PeerAddress) ipString() string {
	return fmt.Sprintf("%s:%d", peerAddress.IP, peerAddress.port)
}

// Try a complete handshake with this peer for the file(s) described by infoHash
func (peer *Peer) makeHandshake(infoHash []byte, needExtension bool) error {
	var handshake Handshake
	if needExtension { // if we need to support extensions, include flags that tell the peer that
		handshake.init(infoHash, PeerConnectionFlagsExtended)
	} else {
		handshake.init(infoHash, PeerConnectionFlagsDefault)
	}

	peer.tcpConnection.SetDeadline(time.Now().Add(PeerTimeout))
	defer peer.tcpConnection.SetDeadline(time.Time{}) // since deadlines are per transaction, automatically close the deadline whenever the function ends (regardless of success)

	_, err := peer.tcpConnection.Write(handshake.encode()) // send the handshake
	if err != nil {
		return fmt.Errorf("couldn't write to connection") // if we can't write to the peer, downloading ain't happening (neglects errors on our end, but those should never happen since the contents of handshake are computer generated)
	}

	remotePeerID, err := peer.decodeResponseAndVerify(infoHash) // get the handshake response and return the remote peer's ID, if successful
	if err != nil {
		return fmt.Errorf("handshake failed") // if the handshake was invalid, discard the peer's info
	}

	if needExtension && peer.supportsExtended { // if we need torrent metadata, and the peer might have it...
		ourErr := peer.makeExtendedHandshake()
		metadataSize, peerErr := peer.completeExtendedHandshake()
		if (peerErr != nil) || (ourErr != nil) { // if the extended handshake fails, we can still use the peer; we just can't get metadata from it
			peer.supportsExtended = false
		} else {
			peer.reportedMetadataSize = metadataSize
		}
	}

	// if we've gotten this far, the handshake was successful, so we can store the remote peer's ID...
	copy(peer.peerID[:], remotePeerID) // ...copy it so we don't store useless reference data to an immutable array...
	return nil                         // ...and return that there was no error
}

func (peer *Peer) makeExtendedHandshake() error {
	messageDict := make(map[string]interface{})
	supportedExtensionsDict := make(map[string]interface{})
	supportedExtensionsDict["ut_metadata"] = LocalMetadataID
	messageDict["m"] = supportedExtensionsDict

	return peer.sendExtendedMessage(0, safeMarshal(messageDict))
}

func (peer *Peer) completeExtendedHandshake() (int64, error) {
	peer.tcpConnection.SetDeadline(time.Now().Add(PeerTimeout))
	defer peer.tcpConnection.SetDeadline(time.Time{}) // since deadlines are per transaction, automatically close the deadline whenever the function ends (regardless of success)

	extendedHandshakeHeader := make([]byte, 6)
	_, err := io.ReadFull(peer.tcpConnection, extendedHandshakeHeader)
	if err != nil {
		return 0, fmt.Errorf("extended error: no extended handshake recieved")
	}

	if extendedHandshakeHeader[4] != ExtendedRequestID {
		return 0, fmt.Errorf("extended error: non-extended bittorrent message ID recieved in message handshake")
	}

	if extendedHandshakeHeader[5] != 0 {
		return 0, fmt.Errorf("extended error: recieved an extended message with id %d before extended handshake", extendedHandshakeHeader[5])
	}

	extendedHandshakeLength := binary.BigEndian.Uint32(extendedHandshakeHeader[:4])
	if extendedHandshakeLength < 2 {
		return 0, fmt.Errorf("extended error: extended handshake length was too short")
	}

	extendedHandshakeData := make([]byte, extendedHandshakeLength-2) // minus two for the bittorrent message ID and the extended message ID
	_, err = io.ReadFull(peer.tcpConnection, extendedHandshakeData)
	if err != nil {
		return 0, fmt.Errorf("extended error: no extended handshake recieved")
	}

	extendedHandshakeDictPrototype, err := bencode.Unmarshal(extendedHandshakeData)
	if err != nil {
		return 0, fmt.Errorf("extended error: couldn't decode extended handshake")
	}

	extendedHandshakeDict, couldConvert := extendedHandshakeDictPrototype.(map[string]interface{})
	if !couldConvert {
		return 0, fmt.Errorf("extended error: extended handshake payload wasn't dict")
	}

	supportedExtensionsDictPrototype := extendedHandshakeDict["m"]
	if supportedExtensionsDictPrototype == nil {
		return 0, fmt.Errorf("extended error: no 'm' key in extended handshake")
	}
	supportedExtensionsDict, couldConvert := supportedExtensionsDictPrototype.(map[string]interface{})
	if !couldConvert {
		return 0, fmt.Errorf("extended error: extended handshake 'm' key wasn't dict")
	}

	extensionIDPrototype := supportedExtensionsDict["ut_metadata"]
	if extensionIDPrototype == nil {
		return 0, fmt.Errorf("extended error: extended handshake didn't support ut_metadata")
	}
	extensionID, couldConvert := extensionIDPrototype.(int64)
	if !couldConvert {
		return 0, fmt.Errorf("extended error: handshake contained invalid ID for ut_metadata")
	}
	peer.metadataExtensionID = uint8(extensionID) // cast is safe because this is transmitted as a byte over the wire, so it can't possibly be larger than this

	metadataSizePrototype := extendedHandshakeDict["metadata_size"]
	if supportedExtensionsDictPrototype == nil {
		return 0, fmt.Errorf("extended error: no 'm' key in extended handshake")
	}
	metadataSize, couldConvert := metadataSizePrototype.(int64)
	if !couldConvert || metadataSize > MaxMetadataSize {
		return 0, fmt.Errorf("extended error: extended handshake had an invalid metadataSize")
	}

	fmt.Printf("successfully did extended handshake with peer %s\n", peer.peerAddress.IP)
	return metadataSize, nil
}

func (peerAddress PeerAddress) connect(peerStatusChannel chan *Peer, infoHash []byte, needExtension bool) {
	var peer Peer // set up the peer to hold all the possible info
	peer.peerAddress = peerAddress

	tcpConnection, err := net.DialTimeout("tcp", peerAddress.ipString(), PeerTimeout)
	if err != nil { // if we can't even create a TCP connection with the peer, downloading ain't happening
		fmt.Printf("failed to connect to peer at %s (TCP connection failed: %s)\n", peer.peerAddress.IP, err)
		peerStatusChannel <- nil // tell the torrent handler that this peer failed
		return
	}
	peer.tcpConnection = tcpConnection

	if peer.makeHandshake(infoHash, needExtension) != nil { // if the handshake failed, we can't download anything from this peer, so they are USELESS
		peer.tcpConnection.Close()
		fmt.Printf("failed to connect to peer at %s (handshake failed)\n", peer.peerAddress.IP)
		peerStatusChannel <- nil
		return
	}

	bitfieldMessage, err := peer.recieveAndDecodeMessage()                              // after a successful handshake, the server should immediately tell the peer which pieces it has by sending a bitfield
	if (err != nil) || (bitfieldMessage == nil) || (bitfieldMessage.ID != BitfieldID) { // if there's no bitfield, assume this peer has no pieces and close it
		peer.tcpConnection.Close()
		fmt.Printf("failed to connect to peer at %s (had no pieces)\n", peer.peerAddress.IP)
		peerStatusChannel <- nil
		return
	}
	peer.bitfield = bitfieldMessage.content

	peer.choked = true // all connections start off choked, per protocol
	if peerStatusChannel != nil {
		peerStatusChannel <- &peer // otherwise, if everything worked, send this peer back to the TorrentHandler and start downloading
	}
}

func (partialPieceData *PartialPieceData) verifyPiece(newData []byte, pieceIndex uint32) (uint32, error) {
	if len(newData) < MinBlockLen {
		return 0, fmt.Errorf("new piece had too few bytes (expected at least %d, got %d)", MinBlockLen, len(newData))
	}

	newPieceIndex := binary.BigEndian.Uint32(newData[0:4])
	if newPieceIndex != pieceIndex { // if this piece is the wrong index...
		return 0, fmt.Errorf("got piece at index %d while expecting index %d", newPieceIndex, pieceIndex) // ... drop the peer (????)
	}

	newDataStartIndex := binary.BigEndian.Uint32(newData[4:8])     // the index in the piece's byte slice where this new data goes
	if newDataStartIndex > uint32(len(partialPieceData.content)) { // check before copying the data because we spend an extra comparison but potentially gain a slice initialization
		return 0, fmt.Errorf("got data starting at index %d when piece had length %d", newDataStartIndex, len(partialPieceData.content))
	}

	newDataBytes := newData[8:]
	if (newDataStartIndex + uint32(len(newDataBytes))) > uint32(len(partialPieceData.content)) { // check before copying the data because we spend an extra comparison but potentially gain a slice initialization
		return 0, fmt.Errorf("got data ending at index %d when piece had length %d", newDataStartIndex+uint32(len(newDataBytes)), len(partialPieceData.content))
	}

	copy(partialPieceData.content[newDataStartIndex:], newDataBytes) // copy is safe, since we just checked length
	return uint32(len(newDataBytes)), nil
}

func (peer *Peer) readNextInfoDictMessage(expectedPieceSize uint64, pieceIndex uint64) (*InfoDictPiece, error) {
	message, err := peer.recieveAndDecodeMessage() // block until there's a response from the remote peer
	if err != nil {                                // if there was an error in the request, drop the peer (could be optimized?)
		return nil, err
	}

	if message == nil { // nil messages are just to keep the tcp connection alive
		return nil, nil
	}

	switch message.ID {
	default: // ignore all unrecognized message IDs
	case ChokeID:
		peer.choked = true
		return nil, fmt.Errorf("choked") // peers that choke us during info dict exchange don't want to info dict exchange with us
	case UnchokeID:
		peer.choked = false
	case HaveID:
		if len(message.content) > 4 {
			return nil, fmt.Errorf("index of have was %d, should be maximum of 4", len(message.content))
		}
		newPieceIndex := binary.BigEndian.Uint32(message.content)
		peer.setHasPiece(newPieceIndex)
	case ExtendedRequestID:
		return peer.handleExtendedMessage(message.content, expectedPieceSize, pieceIndex)
	}

	// if we've gotten this far, everything went all right
	return nil, nil
}

func (peer *Peer) readNextMessage(partialPieceData *PartialPieceData, possiblePieceIndex uint32) (bool, error) {
	message, err := peer.recieveAndDecodeMessage() // block until there's a response from the remote peer
	if err != nil {                                // if there was an error in the request, drop the peer (could be optimized?)
		return false, err
	}

	if message == nil { // nil messages are just to keep the tcp connection alive
		return false, nil
	}

	justChoked := false
	switch message.ID {
	default: // ignore all unrecognized message IDs
	case ChokeID:
		fmt.Printf("got choked from peer %s\n", peer.peerAddress.IP)
		peer.choked = true
		justChoked = true
	case UnchokeID:
		fmt.Printf("got unchoked from peer %s\n", peer.peerAddress.IP)
		peer.choked = false
	case HaveID:
		if len(message.content) > 4 {
			return false, fmt.Errorf("index of have was %d, should be maximum of 4", len(message.content))
		}
		newPieceIndex := binary.BigEndian.Uint32(message.content)
		peer.setHasPiece(newPieceIndex)
	case PieceID:
		nNewBytes, err := partialPieceData.verifyPiece(message.content, possiblePieceIndex) // parse the new piece and copy it into the partialPieceData's byteslice
		if err != nil {                                                                     // if something went wrong while parsing the new piece...
			return false, err // ...drop the peer (could be optimized)
		}
		partialPieceData.bytesDownloaded += nNewBytes
		partialPieceData.numPendingRequests--
	}

	// if we've gotten this far, everything went all right
	return justChoked, nil
}

func (peer *Peer) handleExtendedMessage(messageBytes []byte, expectedPieceSize uint64, pieceIndex uint64) (*InfoDictPiece, error) {
	if !peer.supportsExtended { // ignore extended messages to peers that don't support them
		return nil, nil
	}
	if messageBytes[0] != LocalMetadataID { // ignore extended messages that we don't support (that is, all extended messages that aren't metadata providers)
		return nil, nil
	}

	responseData := messageBytes[1:]
	responseDictEndIndex, err := findResponseDictEnd(responseData) // whichever bumblecunt decided to append the info dict data AFTER the dictionary instead of including it as a value INSIDE the dictionary is going straight to the top of the shank list
	if err != nil {
		return nil, fmt.Errorf("metadata response dict from peer %s doesn't end", peer.peerAddress.IP)
	}

	responseDictPrototype, err := bencode.Unmarshal(responseData[:responseDictEndIndex])
	if err != nil {
		return nil, err
	}
	responseDict, couldConvert := responseDictPrototype.(map[string]interface{})
	if !couldConvert {
		return nil, fmt.Errorf("couldn't convert metadata response from peer %s into dict", peer.peerAddress.IP)
	}

	responseTypePrototype := responseDict["msg_type"]
	if responseTypePrototype == nil {
		return nil, fmt.Errorf("response to metadata request from peer %s didn't have msg_type", peer.peerAddress.IP)
	}
	responseType, couldConvert := responseTypePrototype.(int64)
	if !couldConvert {
		return nil, fmt.Errorf("response to metadata rqeuest from peer %s had invalid msg_type", peer.peerAddress.IP)
	}
	switch uint64(responseType) {
	default:
		return nil, fmt.Errorf("response to metadata request from peer %s had unrecognized response type %d", peer.peerAddress.IP, responseType)
	case InfoDictRejectID:
		return nil, fmt.Errorf("rejected") // this will tell the process to stop requesting pieces from this peer
	case InfoDictDataID: // if we got this, we actually got data!
		if uint64(len(responseData)-responseDictEndIndex) != expectedPieceSize {
			return nil, fmt.Errorf("metadata response from peer %s was expected to have %d bytes; actually had %d", peer.peerAddress.IP, expectedPieceSize, uint64(len(responseData)-responseDictEndIndex))
		}

		// if we've gotten this far, we got a valid response with data, and can now return it
		var newPiece InfoDictPiece
		newPiece.index = pieceIndex
		newPiece.length = uint64(len(responseData) - responseDictEndIndex)
		newPiece.data = responseData[responseDictEndIndex:]
		return &newPiece, nil
	}
}

func (peer *Peer) downloadPiece(piece *UndownloadedPieceInfo) ([]byte, error) {
	partialPiece := PartialPieceData{content: make([]byte, piece.length)}

	for partialPiece.bytesDownloaded < piece.length {
		if !peer.choked {
			for (partialPiece.numPendingRequests < MaxPendingRequests) && (partialPiece.bytesRequested < piece.length) { // keep queueing up requests until we a) have the max number of requests queued or b) have requested all data
				var bytesInNextRequest uint32
				if bytesLeft := piece.length - partialPiece.bytesRequested; bytesLeft < MaxPieceSize { // never request more bytes than the file has remaining
					bytesInNextRequest = bytesLeft
				} else {
					bytesInNextRequest = MaxPieceSize
				}

				err := peer.sendRequest(piece.index, partialPiece.bytesRequested, bytesInNextRequest)
				if err != nil { // if we couldn't write the request
					fmt.Printf("failed to send request to peer %s\n", peer.peerAddress.IP)
					return nil, err // stop downloading from this peer
				}

				partialPiece.numPendingRequests++
				partialPiece.bytesRequested += bytesInNextRequest
			}
		}

		// after we've a) requested as many bytes as we can at a time or b) requested the last bytes in the file, try to read the response
		isChoked, err := peer.readNextMessage(&partialPiece, piece.index)
		if err != nil { // if we couldn't read the response...
			return nil, err // ...wait a little then try again (works a limited number of times, then we just drop the peer)
		}
		if isChoked { // otherwise, if we were just choked, the peer won't fulfill any of our outstanding requests
			return nil, fmt.Errorf("choked") // so put the piece back on the stack and let other peers try it
		}
	}

	// if we've gotten this far, we've successfully downloaded all the content; return it for checksum verification
	return partialPiece.content, nil
}

func (peer *Peer) openConnection() {
	peer.sendUnchoke()
	peer.sendInterested()
}

func (peer *Peer) resetConnection(infoHash []byte) error {
	peer.tcpConnection.Close()

	tcpConnection, err := net.DialTimeout("tcp", peer.peerAddress.ipString(), PeerTimeout)
	if err != nil { // if we can't even create a TCP connection with the peer, downloading ain't happening
		fmt.Printf("failed to connect to peer at %s (TCP connection failed)\n", peer.peerAddress.IP)
		return fmt.Errorf("failed to connect to peer at %s (TCP connection failed: %s)", peer.peerAddress.IP, err)
	}
	peer.tcpConnection = tcpConnection

	if peer.makeHandshake(infoHash, false) != nil { // if the handshake failed, we can't download anything from this peer, so they are USELESS
		peer.tcpConnection.Close()
		fmt.Printf("failed to connect to peer at %s (handshake failed)\n", peer.peerAddress.IP)
		return fmt.Errorf("failed to connect to peer at %s (handshake failed)", peer.peerAddress.IP)
	}

	bitfieldMessage, err := peer.recieveAndDecodeMessage()                              // after a successful handshake, the server should immediately tell the peer which pieces it has by sending a bitfield
	if (err != nil) || (bitfieldMessage == nil) || (bitfieldMessage.ID != BitfieldID) { // if there's no bitfield, assume this peer has no pieces and close it
		peer.tcpConnection.Close()
		fmt.Printf("failed to connect to peer at %s (had no pieces)\n", peer.peerAddress.IP)
		return fmt.Errorf("failed to connect to peer at %s (had no pieces)", peer.peerAddress.IP)
	}
	peer.bitfield = bitfieldMessage.content

	peer.choked = true // all connections start off choked, per protocol
	peer.openConnection()
	return nil
}

func (peer *Peer) startDownload(piecesRemainingChannel chan *UndownloadedPieceInfo, piecesFinishedChannel chan *PieceData, neededExtension bool, infoHash []byte) {
	defer peer.tcpConnection.Close() // when we're done here, close the TCP connection

	fmt.Printf("starting download from peer %s\n", peer.peerAddress.IP)
	if !neededExtension || !peer.supportsExtended { // if we needed metadata from this peer and it supported fetching it, we've already opened the connection (unchoked/interested) with them
		peer.openConnection()
	}

	if peer.choked { // if we were choked during metadata exchange, try to unchoke for data exchange
		peer.sendUnchoke()
	}

	failures := 0
	for pieceToDownload := range piecesRemainingChannel { // while there's pieces left to download, try to download the next piece in the channel
		if !peer.getHasPiece(pieceToDownload.index) { // if we don't have this piece...
			piecesRemainingChannel <- pieceToDownload // ...put it on the bottom of the channel...
			continue                                  // ...and move on to the next piece
		}

		pieceData, err := peer.downloadPiece(pieceToDownload)
		if err != nil { // if the download failed...
			piecesRemainingChannel <- pieceToDownload // ...put the piece on the bottom of the channel...
			if err.Error() != "choked" {              // (if we were choked, that doesn't reflect badly on the peer)
				failures = peer.handleConnectionFailure(infoHash, failures, err)
				if failures == -1 { // this means we've failed too many times and need to drop the peer
					return
				}
			} else {
				fmt.Printf("putting back piece %s because we were choked by peer %s\n", pieceToDownload, peer.peerAddress.IP)
			}
			continue // after waiting, get a new piece and continue
		}

		pieceDataHash := sha1.Sum(pieceData)
		if !bytes.Equal(pieceDataHash[:], pieceToDownload.sha1Hash) { // if the download succeeded, but the checksum isn't right...
			piecesRemainingChannel <- pieceToDownload // ...put the piece on the bottom of the channel...
			continue                                  // ...and move on to the next piece
		}

		// if the download succeeded and the checksum is correct, we can tell our peer we have this file...
		peer.sendHave(pieceToDownload.index)
		fmt.Println("successfully downloaded piece", pieceToDownload.index, "from", peer.peerAddress.IP)

		piecesFinishedChannel <- &PieceData{pieceToDownload.index, pieceData} // ...and send the torrent handler the data
	}
}

func (peer *Peer) handleConnectionFailure(infoHash []byte, failures int, err error) int {
	failures++

	fmt.Printf("failure %d/%d from peer %s: %s\n", failures, MaxFailuresPerPeer, peer.peerAddress.IP, err)
	if failures == MaxFailuresPerPeer {
		fmt.Printf("dropping peer %s\n", peer.peerAddress.IP)
		return -1
	}
	fmt.Printf("sleeping %d seconds, then reconnecting to peer %s\n", failures*PeerFailureBaseSleepTime, peer.peerAddress.IP)
	time.Sleep(time.Duration(failures*PeerFailureBaseSleepTime) * time.Second)
	err = peer.resetConnection(infoHash)
	if err != nil {
		fmt.Printf("error reconnecting to peer %s: %s\n", peer.peerAddress.IP, err)
		return peer.handleConnectionFailure(infoHash, failures, err)
	}

	return failures
}

// getConsensusMetadataSize helps protect againt malicious peers giving us incorrect infoDicts, as well as a way to decide on how many pieces to request to get the infoDict
func getConsensusMetadataSize(peers []*Peer) (uint64, error) {
	reportedMetadataSizes := make(map[int64]int) // map of (reported metadata size) to (how many times this size was reported)
	for _, peer := range peers {
		if (peer != nil) && (peer.reportedMetadataSize > 0) { // peers who don't support metadata sharing or whose extended handshake field have this set to zero by default
			_, exists := reportedMetadataSizes[peer.reportedMetadataSize]
			if !exists {
				reportedMetadataSizes[peer.reportedMetadataSize] = 1
			} else {
				reportedMetadataSizes[peer.reportedMetadataSize]++
			}
		}
	}

	if len(reportedMetadataSizes) == 0 {
		return 0, fmt.Errorf("No nodes we found could provide the necessary metadata on this torrent! (try again in 30 seconds)")
	} else if len(reportedMetadataSizes) == 1 { // if there's only one metadata value (like we hope), just return it
		for size := range reportedMetadataSizes { // slightly hacky way to return the only value
			return uint64(size), nil // cast is safe because metadata size is never negative
		}
	} else {
		maxOccurences := 0
		consensusSize := int64(0)
		for size, occurences := range reportedMetadataSizes { // returns the first node reported by default (could be abused, but only if an attacker has enough peers to make a tie anyway)
			if occurences > maxOccurences {
				maxOccurences = occurences
				consensusSize = size
			}
		}
		return uint64(consensusSize), nil // cast is safe because metadata size is never negative
	}

	return 0, fmt.Errorf("[](/rdwut)")
}

func (peer *Peer) fetchInfoDict(piecesRemainingChannel chan uint64, infoDictPiecesChannel chan *InfoDictPiece, numPieces uint64, lastPieceSize uint64) {
	peer.openConnection() // start by unchoking and signaling interest, so we can send the peer metadata requests

	var expectedPieceSize uint64
	for pieceToDownload := range piecesRemainingChannel { // while there's pieces left to download, try to download the next piece in the channel
		println("trying to download info dict piece ", pieceToDownload)
		if pieceToDownload == (numPieces - 1) { // if this is the last piece (minus one because pieceToDownload is an index)...
			expectedPieceSize = lastPieceSize // ...we expect it to be the size of the last piece
		} else {
			expectedPieceSize = InfoDictPieceSize // otherwise, we expect it to be exactly 16 KiB as per protocol
		}
		peer.sendInfoDictPieceRequest(pieceToDownload)

		println("waiting for response...")
		var recievedPiece *InfoDictPiece
		var err error
		for recievedPiece == nil {
			recievedPiece, err = peer.readNextInfoDictMessage(expectedPieceSize, pieceToDownload) // read and handle messages until we get one giving us info dict data
			if err != nil {                                                                       // if something went wrong (most likely a timeout)...
				fmt.Printf("error while downloading piece %d: %s\n", pieceToDownload, err)

				piecesRemainingChannel <- pieceToDownload // ...put the piece on the bottom of the channel...
				infoDictPiecesChannel <- nil              // ...tell the managing goroutine that this peer won't return anything else...
				return                                    // ... and stop pestering this peer
			}
		}
		fmt.Println("successfully downloaded info dict piece", pieceToDownload, "from", peer.peerAddress.IP)

		infoDictPiecesChannel <- recievedPiece // ...and send the torrent handler the data
	}
	fmt.Printf("finished info dict exchange with peer %s\n", peer.peerAddress.IP)
}

func (peer *Peer) sendInfoDictPieceRequest(pieceIndex uint64) error {
	downloadRequest := make(map[string]interface{})
	downloadRequest["msg_type"] = InfoDictRequestID
	downloadRequest["piece"] = pieceIndex

	err := peer.sendExtendedMessage(peer.metadataExtensionID, safeMarshal(downloadRequest))
	if err != nil {
		fmt.Printf("couldn't send request for metadata to peer %s\n", peer.peerAddress.IP)
	} else {
		fmt.Printf("sent request for metadata to peer %s\n", peer.peerAddress.IP)
	}
	return err
}

func (peer *Peer) downloadInfoDictPiece(pieceIndex uint64, expectedPieceSize uint64) (*InfoDictPiece, error) {
	downloadRequest := make(map[string]interface{})
	downloadRequest["msg_type"] = InfoDictRequestID
	downloadRequest["piece"] = pieceIndex

	err := peer.sendExtendedMessage(peer.metadataExtensionID, safeMarshal(downloadRequest))
	if err != nil {
		return nil, err
	}

	peer.tcpConnection.SetDeadline(time.Now().Add(PieceTimeout))
	defer peer.tcpConnection.SetDeadline(time.Time{}) // per protocol, deadlines should be set per transaction, so reset the deadline when we're done here

	responseHeader := make([]byte, 6)
	_, err = io.ReadFull(peer.tcpConnection, responseHeader)
	if err != nil {
		return nil, err
	}

	/*if responseHeader[5] != peer.metadataExtensionID {
		return nil, fmt.Errorf("peer %s replied to metadata request with an unfamiliar ID %d (expected %d)", peer.peerAddress.IP, responseHeader[4], peer.metadataExtensionID)
	}*/

	responseData := make([]byte, binary.BigEndian.Uint32(responseHeader[:4])-2) // minus 1 because the message length includes the 1-byte id that says we're using extended protocols and the 1-byte extended protocol id
	_, err = io.ReadFull(peer.tcpConnection, responseData)
	if err != nil {
		return nil, err
	}
	fmt.Printf("response data from peer %s: %s\n", peer.peerAddress.IP, responseData)

	responseDictEndIndex, err := findResponseDictEnd(responseData) // whichever bumblecunt decided to append the info dict data AFTER the dictionary instead of including it as a value INSIDE the dictionary is going straight to the top of the shank list
	if err != nil {
		return nil, fmt.Errorf("metadata response dict from peer %s doesn't end", peer.peerAddress.IP)
	}

	responseDictPrototype, err := bencode.Unmarshal(responseData[:responseDictEndIndex])
	if err != nil {
		return nil, err
	}
	responseDict, couldConvert := responseDictPrototype.(map[string]interface{})
	if !couldConvert {
		return nil, fmt.Errorf("couldn't convert metadata response from peer %s into dict", peer.peerAddress.IP)
	}

	responseTypePrototype := responseDict["msg_type"]
	if responseTypePrototype == nil {
		return nil, fmt.Errorf("response to metadata request from peer %s didn't have msg_type", peer.peerAddress.IP)
	}
	responseType, couldConvert := responseTypePrototype.(int64)
	if !couldConvert {
		return nil, fmt.Errorf("response to metadata rqeuest from peer %s had invalid msg_type", peer.peerAddress.IP)
	}
	switch uint64(responseType) {
	default:
		return nil, fmt.Errorf("response to metadata request from peer %s had unrecognized response type %d", peer.peerAddress.IP, responseType)
	case InfoDictRejectID:
		return nil, fmt.Errorf("rejected") // this will tell the process to stop requesting pieces from this peer
	case InfoDictDataID: // if we got this, we actually got data!
		if uint64(len(responseData)-responseDictEndIndex) != expectedPieceSize {
			return nil, fmt.Errorf("metadata response from peer %s was expected to have %d bytes; actually had %d", peer.peerAddress.IP, expectedPieceSize, uint64(len(responseData)-responseDictEndIndex))
		}

		// if we've gotten this far, we got a valid response with data, and can now return it
		var newPiece InfoDictPiece
		newPiece.index = pieceIndex
		newPiece.length = uint64(len(responseData) - responseDictEndIndex)
		newPiece.data = responseData[responseDictEndIndex:]
		return &newPiece, nil
	}
}

func findResponseDictEnd(responseData []byte) (int, error) {
	for a := 0; a < len(responseData)-1; a++ { // minus 1 because we check the a'th byte and the (a+1)'th byte
		if responseData[a] == 101 && responseData[a+1] == 101 { // the response dict is a dictionary of ints, thus, the first 'ee' we come across is (end of the last int, end of the dict)
			return a + 2, nil // if the a'th byte and the (a+1)'th byte end the response dict, then the beginning of the next data starts at the (a+2)'th byte
		}
	}

	return 0, fmt.Errorf("response dict doesn't end")
}
