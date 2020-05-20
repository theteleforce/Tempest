package main

import (
	"math/rand"
	"time"
)

// Constants //

// IPCQueueSize is the maximum number of requests that can be pending on one subprocess at once
const IPCQueueSize = 1024

// IPCRequestIDSize is the number of bytes in an IPCRequestID
const IPCRequestIDSize = 20 // set as 20 so we can use torrent info hashes as request IDs

// IPCTimeout is the generic timeout on IPC requests
const IPCTimeout = float64(3)

// SubprocessSleepTime is the time a subprocess sleeps if it has no pending work
const SubprocessSleepTime = time.Second / 10

// Structs //

// TorrentIPCChannels stores the channels the torrent client uses to communicate with other processes
type TorrentIPCChannels struct {
	outgoingDhtRequestChannel chan *IPCRequest
	incomingDhtRequestChannel chan *IPCRequest
	outgoingDhtReplyChannel   chan *IPCReply
	incomingDhtReplyChannel   chan *IPCReply
	outgoingGuiRequestChannel chan *IPCRequest
	incomingGuiRequestChannel chan *IPCRequest
	outgoingGuiReplyChannel   chan *IPCReply
	incomingGuiReplyChannel   chan *IPCReply
}

// DhtIPCChannels stores the channels the DHT client uses to communicate with other processes
type DhtIPCChannels struct {
	outgoingTorrentRequestChannel chan *IPCRequest
	incomingTorrentRequestChannel chan *IPCRequest
	outgoingTorrentReplyChannel   chan *IPCReply
	incomingTorrentReplyChannel   chan *IPCReply
	outgoingGuiRequestChannel     chan *IPCRequest
	incomingGuiRequestChannel     chan *IPCRequest
	outgoingGuiReplyChannel       chan *IPCReply
	incomingGuiReplyChannel       chan *IPCReply
}

// GuiIPCChannels stores the channels the torrent client uses to communicate with other processes
type GuiIPCChannels struct {
	outgoingTorrentRequestChannel chan *IPCRequest
	incomingTorrentRequestChannel chan *IPCRequest
	outgoingTorrentReplyChannel   chan *IPCReply
	incomingTorrentReplyChannel   chan *IPCReply
	outgoingDhtRequestChannel     chan *IPCRequest
	incomingDhtRequestChannel     chan *IPCRequest
	outgoingDhtReplyChannel       chan *IPCReply
	incomingDhtReplyChannel       chan *IPCReply
}

// IPCRequest represents a request from one subroutine to do something in another
type IPCRequest struct {
	messageID    []byte
	functionCall string
	args         []interface{}
}

// IPCReply represents a reply to a request from one subroutine to another
type IPCReply struct {
	messageID    []byte
	returnValues []interface{}
	err          error
}

// Functions //
func createIPCChannels() (*TorrentIPCChannels, *DhtIPCChannels, *GuiIPCChannels) {
	torrentToDhtRequest := make(chan *IPCRequest, IPCQueueSize)
	torrentToDhtReply := make(chan *IPCReply, IPCQueueSize)
	dhtToTorrentRequest := make(chan *IPCRequest, IPCQueueSize)
	dhtToTorrentReply := make(chan *IPCReply, IPCQueueSize)

	torrentToGuiRequest := make(chan *IPCRequest, IPCQueueSize)
	torrentToGuiReply := make(chan *IPCReply, IPCQueueSize)
	guiToTorrentRequest := make(chan *IPCRequest, IPCQueueSize)
	guiToTorrentReply := make(chan *IPCReply, IPCQueueSize)

	guiToDhtRequest := make(chan *IPCRequest, IPCQueueSize)
	guiToDhtReply := make(chan *IPCReply, IPCQueueSize)
	dhtToGuiRequest := make(chan *IPCRequest, IPCQueueSize)
	dhtToGuiReply := make(chan *IPCReply, IPCQueueSize)

	torrentChannels := &TorrentIPCChannels{torrentToDhtRequest, dhtToTorrentRequest, torrentToDhtReply, dhtToTorrentReply, torrentToGuiRequest, guiToTorrentRequest, torrentToGuiReply, guiToTorrentReply}
	dhtChannels := &DhtIPCChannels{dhtToTorrentRequest, torrentToDhtRequest, dhtToTorrentReply, torrentToDhtReply, dhtToGuiRequest, guiToDhtRequest, dhtToGuiReply, guiToDhtReply}
	guiChannels := &GuiIPCChannels{guiToTorrentRequest, torrentToGuiRequest, guiToTorrentReply, torrentToGuiReply, guiToDhtRequest, dhtToGuiRequest, guiToDhtReply, dhtToGuiReply}
	return torrentChannels, dhtChannels, guiChannels
}

func (request *IPCRequest) genMessageID() {
	request.messageID = make([]byte, IPCRequestIDSize)
	rand.Read(request.messageID)
}
